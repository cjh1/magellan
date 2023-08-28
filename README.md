# Magellan

Magellan is a small tool designed to collect BMC information and load the data
into `hms-smd`. It is able to probe hosts for specific open ports using the `dora`
API or it's own simplier, built-in scanner and query BMC information via `bmclib`.
Once the data is received, it is then stored into `hms-smd` using its API.

## Building

To build the project, run the following:

```bash
go mod tidy && go build
```

## Usage

Magellan can be used to load inventory components using redfish or IMPI interfaces.
It can scan subnets or specific hosts to find...

```bash
./magellan --help
Usage of ./magellan:
      --cert-pool string   path to an file containing x509 CAs. An empty string uses the system CAs. Only takes effect when --secure-tls=true
      --driver strings     set the BMC driver to use (default [redfish])
      --host strings       set additional hosts
      --pass string        set the BMC pass (default "root_password")
      --port ints          set the ports to scan
      --secure-tls         enable secure TLS
      --subnet strings     set additional subnets (default [127.0.0.0])
      --threads int        set the number of threads (default -1)
      --timeout int        set the timeout (default 1)
      --user string        set the BMC user (default "root")

# example usage
magellan \
    --subnet 127.0.0.0 \
    --host 127.0.0.1 \
    --port 5000 \
    --timeout 10 \
    --user root \
    --pass root_password \
    --threads 255 \
```

## TODO

List of things left to fix or do...

* [ ] Switch to internal scanner if `dora` fails
* [ ] Test using different `bmclib` supported drivers (mainly 'redfish')
* [ ] Confirm loading different components into `hms-smd`
