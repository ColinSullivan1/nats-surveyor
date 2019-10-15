# NATS Surveyor
A NATS Monitoring POC

This project uses the polling of Statz to generate data for prometheus.

```Usage of ./nats-surveyor:
  -a string
    	Network host to listen on. (default "0.0.0.0")
  -addr string
    	Network host to listen on. (default "0.0.0.0")
  -c int
    	Expected number of servers (default 1)
  -creds string
    	Credentials File
  -http_pass string
    	Set the password for HTTP scrapes. NATS bcrypt supported.
  -http_user string
    	Enable basic auth and set user name for HTTP scrapes.
  -p int
    	Port to listen on. (default 7777)
  -port int
    	Port to listen on. (default 7777)
  -prefix string
    	Replace the default prefix for all the metrics.
  -s string
    	NATS Cluster url(s) (default "nats://localhost:4222")
  -timeout duration
    	Polling timeout (default 3s)
  -tlscacert string
    	Client certificate CA for verification (used with HTTPS).
  -tlscert string
    	Server certificate file (Enables HTTPS).
  -tlskey string
    	Private key for server certificate (used with HTTPS).
  -version
    	Show exporter version and exit.
```

Example output:

