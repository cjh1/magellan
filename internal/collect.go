package magellan

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path"
	"sync"
	"time"

	"github.com/bikeshack/magellan/internal/log"

	"github.com/bikeshack/magellan/internal/api/smd"
	"github.com/bikeshack/magellan/internal/util"

	"github.com/Cray-HPE/hms-xname/xnames"
	bmclib "github.com/bmc-toolbox/bmclib/v2"
	"github.com/jacobweinstock/registrar"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stmcginnis/gofish"
	_ "github.com/stmcginnis/gofish"
	"github.com/stmcginnis/gofish/redfish"
	"golang.org/x/exp/slices"
)

const (
	IPMI_PORT  = 623
	SSH_PORT   = 22
	HTTPS_PORT = 443
)

type BMCProbeResult struct {
	Host     string `json:"host"`
	Port     int    `json:"port"`
	Protocol string `json:"protocol"`
	State    bool   `json:"state"`
}

// NOTE: ...params were getting too long...
type QueryParams struct {
	Host          string
	Port          int
	User          string
	Pass          string
	Drivers       []string
	Threads       int
	Preferred     string
	Timeout       int
	WithSecureTLS bool
	CertPoolFile  string
	Verbose       bool
	IpmitoolPath  string
	OutputPath    string
}

func NewClient(l *log.Logger, q *QueryParams) (*bmclib.Client, error) {
	// NOTE: bmclib.NewClient(host, port, user, pass)
	// ...seems like the `port` params doesn't work like expected depending on interface

	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	httpClient := http.Client{
		Transport: tr,
	}

	// init client
	clientOpts := []bmclib.Option{
		// bmclib.WithSecureTLS(nil),
		bmclib.WithHTTPClient(&httpClient),
		// bmclib.WithLogger(),
		bmclib.WithRedfishHTTPClient(&httpClient),
		// bmclib.WithDellRedfishUseBasicAuth(true),
		bmclib.WithRedfishPort(fmt.Sprint(q.Port)),
		bmclib.WithRedfishUseBasicAuth(true),
		bmclib.WithIpmitoolPort(fmt.Sprint(IPMI_PORT)),
		bmclib.WithIpmitoolPath(q.IpmitoolPath),
	}

	// only work if valid cert is provided
	if q.WithSecureTLS {
		var pool *x509.CertPool
		if q.CertPoolFile != "" {
			pool = x509.NewCertPool()
			data, err := os.ReadFile(q.CertPoolFile)
			if err != nil {
				return nil, fmt.Errorf("could not read cert pool file: %v", err)
			}
			pool.AppendCertsFromPEM(data)
		}
		// a nil pool uses the system certs
		clientOpts = append(clientOpts, bmclib.WithSecureTLS(pool))
	}
	url := ""
	if q.User != "" && q.Pass != "" {
		url += fmt.Sprintf("https://%s:%s@%s", q.User, q.Pass, q.Host)
	} else {
		url += q.Host
	}
	client := bmclib.NewClient(url, q.User, q.Pass, clientOpts...)
	ds := registrar.Drivers{}
	for _, driver := range q.Drivers {
		ds = append(ds, client.Registry.Using(driver)...) // ipmi, gofish, redfish
	}
	client.Registry.Drivers = ds

	return client, nil
}

func CollectInfo(probeStates *[]BMCProbeResult, l *log.Logger, q *QueryParams) error {
	// check for available probe states
	if probeStates == nil {
		return fmt.Errorf("no probe states found")
	}
	if len(*probeStates) <= 0 {
		return fmt.Errorf("no probe states found")
	}

	// generate custom xnames for bmcs
	node := xnames.Node{
		Cabinet:       1000,
		Chassis:       1,
		ComputeModule: 7,
		NodeBMC:       -1,
	}

	// make the output directory to store files
	outputPath := path.Clean(q.OutputPath)
	outputPath, err := util.MakeOutputDirectory(outputPath)
	if err != nil {
		l.Log.Errorf("could not make output directory: %v", err)
	}

	found := make([]string, 0, len(*probeStates))
	done := make(chan struct{}, q.Threads+1)
	chanProbeState := make(chan BMCProbeResult, q.Threads+1)

	// collect bmc information asynchronously
	var wg sync.WaitGroup
	wg.Add(q.Threads)
	for i := 0; i < q.Threads; i++ {
		go func() {
			for {
				ps, ok := <-chanProbeState
				if !ok {
					wg.Done()
					return
				}
				q.Host = ps.Host
				q.Port = ps.Port

				client, err := NewClient(l, q)
				if err != nil {
					l.Log.Errorf("could not make client: %v", err)
					continue
				}

				// data to be sent to smd
				data := make(map[string]any)
				data["ID"] = fmt.Sprintf("%v", node.String()[:len(node.String())-2])
				data["Type"] = ""
				data["Name"] = ""
				data["FQDN"] = ps.Host
				data["User"] = q.User
				data["Password"] = q.Pass
				data["RediscoverOnUpdate"] = false

				// unmarshal json to send in correct format
				var rm map[string]json.RawMessage

				// inventories
				inventory, err := QueryInventory(client, l, q)
				if err != nil {
					l.Log.Errorf("could not query inventory (%v:%v): %v", q.Host, q.Port, err)
				}
				json.Unmarshal(inventory, &rm)
				data["Inventory"] = rm["Inventory"]

				// chassis
				chassis, err := QueryChassis(q)
				if err != nil {
					l.Log.Errorf("could not query chassis: %v", err)
					continue
				}
				json.Unmarshal(chassis, &rm)
				data["Chassis"] = rm["Chassis"]

				// ethernet interfaces
				interfaces, err := QueryEthernetInterfaces(client, l, q)
				if err != nil {
					l.Log.Errorf("could not query ethernet interfaces: %v", err)
					continue
				}
				json.Unmarshal(interfaces, &rm)
				data["Interface"] = rm["Interface"]

				// storage
				// storage, err := QueryStorage(q)
				// if err != nil {
				// 	l.Log.Errorf("could not query storage: %v", err)
				// 	continue
				// }
				// json.Unmarshal(storage, &rm)
				// data["Storage"] = rm["Storage"]

				// systems
				systems, err := QuerySystems(q)
				if err != nil {
					l.Log.Errorf("could not query systems: %v", err)
				}
				json.Unmarshal(systems, &rm)
				data["Systems"] = rm["Systems"]

				// registries
				// registries, err := QueryRegisteries(q)
				// if err != nil {
				// 	l.Log.Errorf("could not query registries: %v", err)
				// }
				// json.Unmarshal(registries, &rm)
				// data["Registries"] = rm["Registries"]

				node.NodeBMC += 1

				headers := make(map[string]string)
				headers["Content-Type"] = "application/json"

				b, err := json.MarshalIndent(data, "", "    ")
				if err != nil {
					l.Log.Errorf("could not marshal JSON: %v", err)
				}

				// write JSON data to file
				err = os.WriteFile(path.Clean(outputPath + "/" + q.Host + ".json"), b, os.ModePerm)
				if err != nil {
					l.Log.Errorf("could not write data to file: %v", err)
				}

				// add all endpoints to smd
				err = smd.AddRedfishEndpoint(b, headers)
				if err != nil {
					l.Log.Errorf("could not add redfish endpoint: %v", err)

					// try updating instead
				}

				// users
				// user, err := magellan.QueryUsers(client, l, &q)
				// if err != nil {
				// 	l.Log.Errorf("could not query users: %v\n", err)
				// }
				// users = append(users, user)

				// bios
				// _, err = magellan.QueryBios(client, l, &q)
				// if err != nil {
				// 	l.Log.Errorf("could not query bios: %v\n", err)
				// }

				// _, err = magellan.QueryPowerState(client, l, &q)
				// if err != nil {
				// 	l.Log.Errorf("could not query power state: %v\n", err)
				// }

				// got host information, so add to list of already probed hosts
				found = append(found, ps.Host)
			}
		}()
	}

	// use the found results to query bmc information
	for _, ps := range *probeStates {
		// skip if found info from host
		foundHost := slices.Index(found, ps.Host)
		if !ps.State || foundHost >= 0 {
			continue
		}
		chanProbeState <- ps
	}

	// handle goroutine paths
	go func() {
		select {
		case <-done:
			wg.Done()
			break
		default:
			time.Sleep(1000)
		}
	}()

	close(chanProbeState)
	wg.Wait()
	close(done)
	return nil
}

func QueryMetadata(client *bmclib.Client, l *log.Logger, q *QueryParams) ([]byte, error) {
	// client, err := NewClient(l, q)

	// open BMC session and update driver registry
	ctx, ctxCancel := context.WithTimeout(context.Background(), time.Second*time.Duration(q.Timeout))
	client.Registry.FilterForCompatible(ctx)
	err := client.Open(ctx)
	if err != nil {
		ctxCancel()
		return nil, fmt.Errorf("could not connect to bmc: %v", err)
	}

	defer client.Close(ctx)

	metadata := client.GetMetadata()
	if err != nil {
		ctxCancel()
		return nil, fmt.Errorf("could not get metadata: %v", err)
	}

	// retrieve inventory data
	b, err := json.MarshalIndent(metadata, "", "    ")
	if err != nil {
		ctxCancel()
		return nil, fmt.Errorf("could not marshal JSON: %v", err)
	}

	if q.Verbose {
		fmt.Printf("Metadata: %v\n", string(b))
	}
	ctxCancel()
	return b, nil
}

func QueryInventory(client *bmclib.Client, l *log.Logger, q *QueryParams) ([]byte, error) {
	// open BMC session and update driver registry
	ctx, ctxCancel := context.WithTimeout(context.Background(), time.Second*time.Duration(q.Timeout))
	client.Registry.FilterForCompatible(ctx)
	err := client.PreferProvider(q.Preferred).Open(ctx)
	if err != nil {
		ctxCancel()
		return nil, fmt.Errorf("could not open client: %v", err)
	}
	defer client.Close(ctx)

	inventory, err := client.Inventory(ctx)
	if err != nil {
		ctxCancel()
		return nil, fmt.Errorf("could not get inventory: %v", err)
	}

	
	// retrieve inventory data
	data := map[string]any{"Inventory": inventory}
	b, err := json.MarshalIndent(data, "", "    ")
	if err != nil {
		ctxCancel()
		return nil, fmt.Errorf("could not marshal JSON: %v", err)
	}

	if q.Verbose {
		fmt.Printf("%v\n", string(b))
	}
	ctxCancel()
	return b, nil
}

func QueryPowerState(client *bmclib.Client, l *log.Logger, q *QueryParams) ([]byte, error) {
	ctx, ctxCancel := context.WithTimeout(context.Background(), time.Second*time.Duration(q.Timeout))
	client.Registry.FilterForCompatible(ctx)
	err := client.PreferProvider(q.Preferred).Open(ctx)
	if err != nil {
		ctxCancel()
		return nil, fmt.Errorf("could not open client: %v", err)
	}
	defer client.Close(ctx)

	powerState, err := client.GetPowerState(ctx)
	if err != nil {
		ctxCancel()
		return nil, fmt.Errorf("could not get inventory: %v", err)
	}

	// retrieve inventory data
	data := map[string]any{"PowerState": powerState}
	b, err := json.MarshalIndent(data, "", "    ")
	if err != nil {
		ctxCancel()
		return nil, fmt.Errorf("could not marshal JSON: %v", err)
	}

	if q.Verbose {
		fmt.Printf("%v\n", string(b))
	}
	ctxCancel()
	return b, nil

}

func QueryUsers(client *bmclib.Client, l *log.Logger, q *QueryParams) ([]byte, error) {
	// open BMC session and update driver registry
	ctx, ctxCancel := context.WithTimeout(context.Background(), time.Second*time.Duration(q.Timeout))
	client.Registry.FilterForCompatible(ctx)
	err := client.Open(ctx)
	if err != nil {
		ctxCancel()
		return nil, fmt.Errorf("could not connect to bmc: %v", err)
	}

	defer client.Close(ctx)

	users, err := client.ReadUsers(ctx)
	if err != nil {
		ctxCancel()
		return nil, fmt.Errorf("could not get users: %v", err)
	}

	// retrieve inventory data
	data := map[string]any {"Users": users}
	b, err := json.MarshalIndent(data, "", "    ")
	if err != nil {
		ctxCancel()
		return nil, fmt.Errorf("could not marshal JSON: %v", err)
	}

	// return b, nil
	ctxCancel()
	if q.Verbose {
		fmt.Printf("%v\n", string(b))
	}
	return b, nil
}

func QueryBios(client *bmclib.Client, l *log.Logger, q *QueryParams) ([]byte, error) {
	// client, err := NewClient(l, q)
	// if err != nil {
	// 	return nil, fmt.Errorf("could not make query: %v", err)
	// }
	b, err := makeRequest(client, client.GetBiosConfiguration, q.Timeout)
	if q.Verbose {
		fmt.Printf("%v\n", string(b))
	}
	return b, err
}

func QueryEthernetInterfaces(client *bmclib.Client, l *log.Logger, q *QueryParams) ([]byte, error) {
	c, err := connectGofish(q)
	if err != nil {
		return nil, fmt.Errorf("could not connect to bmc: %v", err)
	}

	interfaces, err := redfish.ListReferencedEthernetInterfaces(c, "/redfish/v1/Systems/")
	if err != nil {
		return nil, fmt.Errorf("could not get ethernet interfaces: %v", err)
	}

	data := map[string]any{"Interfaces": interfaces}
	b, err := json.MarshalIndent(data, "", "    ")
	if err != nil {
		return nil, fmt.Errorf("could not marshal JSON: %v", err)
	}

	if q.Verbose {
		fmt.Printf("%v\n", string(b))
	}
	return b, nil
}

func QueryChassis(q *QueryParams) ([]byte, error) {
	c, err := connectGofish(q)
	if err != nil {
		return nil, fmt.Errorf("could not connect to bmc (%v:%v): %v", q.Host, q.Port, err)
	}
	chassis, err := c.Service.Chassis()
	if err != nil {
		return nil, fmt.Errorf("could not query chassis (%v:%v): %v", q.Host, q.Port, err)
	}

	data := map[string]any{"Chassis": chassis}
	b, err := json.MarshalIndent(data, "", "    ")
	if err != nil {
		return nil, fmt.Errorf("could not marshal JSON: %v", err)
	}

	if q.Verbose {
		fmt.Printf("%v\n", string(b))
	}
	return b, nil
}

func QueryStorage(q *QueryParams) ([]byte, error) {
	c, err := connectGofish(q)
	if err != nil {
		return nil, fmt.Errorf("could not connect to bmc (%v:%v): %v", q.Host, q.Port, err)
	}

	systems, err := c.Service.StorageSystems()
	if err != nil {
		return nil, fmt.Errorf("could not query storage systems (%v:%v): %v", q.Host, q.Port, err)
	}

	services, err := c.Service.StorageServices()
	if err != nil {
		return nil, fmt.Errorf("could not query storage services (%v:%v): %v", q.Host, q.Port, err)
	}

	data := map[string]any{
		"Storage": map[string]any{
			"Systems": systems,
			"Services": services,
		},
	}
	b, err := json.MarshalIndent(data, "", "    ")
	if err != nil {
		return nil, fmt.Errorf("could not marshal JSON: %v", err)
	}

	if q.Verbose {
		fmt.Printf("%v\n", string(b))
	}
	return b, nil
}

func QuerySystems(q *QueryParams) ([]byte, error) {
	c, err := connectGofish(q)
	if err != nil {
		return nil, fmt.Errorf("could not connect to bmc (%v:%v): %v", q.Host, q.Port, err)
	}

	systems, err := c.Service.Systems()
	if err != nil {
		return nil, fmt.Errorf("could not query storage systems (%v:%v): %v", q.Host, q.Port, err)
	}

	data := map[string]any{"Systems": systems }
	b, err := json.MarshalIndent(data, "", "    ")
	if err != nil {
		return nil, fmt.Errorf("could not marshal JSON: %v", err)
	}

	if q.Verbose {
		fmt.Printf("%v\n", string(b))
	}
	return b, nil
}

func QueryRegisteries(q *QueryParams) ([]byte, error) {
	c, err := connectGofish(q)
	if err != nil {
		return nil, fmt.Errorf("could not connect to bmc (%v:%v): %v", q.Host, q.Port, err)
	}

	registries, err := c.Service.Registries()
	if err != nil {
		return nil, fmt.Errorf("could not query storage systems (%v:%v): %v", q.Host, q.Port, err)
	}

	data := map[string]any{"Registries": registries }
	b, err := json.MarshalIndent(data, "", "    ")
	if err != nil {
		return nil, fmt.Errorf("could not marshal JSON: %v", err)
	}

	if q.Verbose {
		fmt.Printf("%v\n", string(b))
	}
	return b, nil
}

func connectGofish(q *QueryParams) (*gofish.APIClient, error) {
	config := makeGofishConfig(q)
	c, err := gofish.Connect(config)
	c.Service.ProtocolFeaturesSupported = gofish.ProtocolFeaturesSupported{
		ExpandQuery: gofish.Expand{
			ExpandAll: true,
			Links: true,
		},
	}
	return c, err
}

func makeGofishConfig(q *QueryParams) gofish.ClientConfig {
	url := "https://"
	if q.User != "" && q.Pass != "" {
		url += fmt.Sprintf("%s:%s@", q.User, q.Pass)
	}
	url += fmt.Sprintf("%s:%d", q.Host, q.Port)
  
	return gofish.ClientConfig{
		Endpoint:            url,
		Username:            q.User,
		Password:            q.Pass,
		Insecure:            !q.WithSecureTLS,
		TLSHandshakeTimeout: q.Timeout,
	}
}

func makeRequest[T any](client *bmclib.Client, fn func(context.Context) (T, error), timeout int) ([]byte, error) {
	ctx, ctxCancel := context.WithTimeout(context.Background(), time.Second*time.Duration(timeout))
	client.Registry.FilterForCompatible(ctx)
	err := client.Open(ctx)
	if err != nil {
		ctxCancel()
		return nil, fmt.Errorf("could not open client: %v", err)
	}

	defer client.Close(ctx)

	response, err := fn(ctx)
	if err != nil {
		ctxCancel()
		return nil, fmt.Errorf("could not get response: %v", err)
	}

	ctxCancel()
	return makeJson(response)
}

func makeJson(object any) ([]byte, error) {
	b, err := json.MarshalIndent(object, "", "    ")
	if err != nil {
		return nil, fmt.Errorf("could not marshal JSON: %v", err)
	}
	return []byte(b), nil
}
