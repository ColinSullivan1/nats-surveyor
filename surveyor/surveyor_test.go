// Copyright 2019 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package surveyor is used to garner data from a NATS deployment for Prometheus
package surveyor

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"testing"
	"time"

	nats "github.com/nats-io/go-nats"
	ns "github.com/nats-io/nats-server/server"
)

func StartServer(t *testing.T, confFile string) *ns.Server {
	opts, err := ns.ProcessConfigFile(confFile)
	if err != nil {
		t.Fatalf("Error processing config file: %v", err)
	}

	s, err := ns.NewServer(opts)
	if err != nil || s == nil {
		panic(fmt.Sprintf("No NATS Server object returned: %v", err))
	}

	if !opts.NoLog {
		s.ConfigureLogger()
	}

	// Run server in Go routine.
	go s.Start()

	// Wait for accept loop(s) to be started
	if !s.ReadyForConnections(10 * time.Second) {
		panic("Unable to start NATS Server in Go Routine")
	}
	return s
}

type SuperCluster struct {
	servers []*ns.Server
	clients []*nats.Conn
}

var configFiles = []string{"../test/r1s1.conf", "../test/r1s2.conf", "../test/r2s1.conf"}

const (
	clientCert      = "../test/certs/client-cert.pem"
	clientKey       = "../test/certs/client-key.pem"
	serverCert      = "../test/certs/server-cert.pem"
	serverKey       = "../test/certs/server-key.pem"
	caCertFile      = "../test/certs/ca.pem"
	expectedClients = 3
)

// This function creates a small supercluster for testing, with one client per server.
func NewSuperCluster(t *testing.T) *SuperCluster {
	sc := &SuperCluster{}
	//var servers = make([]*ns.Server, 0)
	//var clients = make([]*nats.Conn, 0)
	for _, f := range configFiles {
		sc.servers = append(sc.servers, StartServer(t, f))
	}

	return sc
}

func (sc *SuperCluster) Shutdown() {
	for _, c := range sc.clients {
		c.Close()
	}
	for _, s := range sc.servers {
		s.Shutdown()
	}
}

func (sc *SuperCluster) SetupClientsAndVerify(t *testing.T) {

	for _, s := range sc.servers {
		c, err := nats.Connect(s.ClientURL(), nats.UserCredentials("../test/myuser.creds"))
		if err != nil {
			t.Fatalf("Couldn't connect a client to %s: %v", s.ClientURL(), err)
		}
		_, err = c.Subscribe("test.ready", func(msg *nats.Msg) {
			c.Publish(msg.Reply, nil)
		})
		if err != nil {
			t.Fatalf("Couldn't subscribe to \"test.ready\": %v", err)
		}
		_, err = c.Subscribe(("test.data"), func(msg *nats.Msg) {
			c.Publish(msg.Reply, []byte("response"))
		})
		if err != nil {
			t.Fatalf("Couldn't subscribe to \"test.data\": %v", err)
		}
	}

	// now poll until we get responses from all subscribers.
	c := sc.clients[0]
	inbox := nats.NewInbox()
	s, err := c.SubscribeSync(inbox)
	if err != nil {
		t.Fatalf("couldn't subscribe to test data:  %v", err)
	}
	var j int

	for i := 0; i < 10; i++ {
		c.PublishRequest("test.ready", inbox, nil)
		for j = 0; j < expectedClients; j++ {
			_, err := s.NextMsg(time.Second * 5)
			if err != nil {
				break
			}
		}
		if j == expectedClients {
			break
		}
	}
	if j != expectedClients {
		t.Fatalf("couldn't ensure the supercluster was formed")
	}
}

func httpGetSecure(url string) (*http.Response, error) {
	tlsConfig := &tls.Config{}
	caCert, err := ioutil.ReadFile(caCertFile)
	if err != nil {
		return nil, fmt.Errorf("Got error reading RootCA file: %s", err)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)
	tlsConfig.RootCAs = caCertPool

	cert, err := tls.LoadX509KeyPair(
		clientCert,
		clientKey)
	if err != nil {
		return nil, fmt.Errorf("Got error reading client certificates: %s", err)
	}
	tlsConfig.Certificates = []tls.Certificate{cert}
	transport := &http.Transport{TLSClientConfig: tlsConfig}
	httpClient := &http.Client{Transport: transport, Timeout: 30 * time.Second}
	return httpClient.Get(url)
}

func httpGet(url string) (*http.Response, error) {
	httpClient := &http.Client{Timeout: 30 * time.Second}
	return httpClient.Get(url)
}

func buildExporterURL(user, pass, addr string, path string, secure bool) string {
	proto := "http"
	if secure {
		proto = "https"
	}

	if user != "" {
		return fmt.Sprintf("%s://%s:%s@%s%s", proto, user, pass, addr, path)
	}

	return fmt.Sprintf("%s://%s%s", proto, addr, path)
}

func checkExporterFull(t *testing.T, user, pass, addr, result, path string, secure bool, expectedRc int) (string, error) {
	var resp *http.Response
	var err error
	url := buildExporterURL(user, pass, addr, path, secure)

	if secure {
		resp, err = httpGetSecure(url)
	} else {
		resp, err = httpGet(url)
	}
	if err != nil {
		return "", fmt.Errorf("error from get: %v", err)
	}
	defer resp.Body.Close()

	rc := resp.StatusCode
	if rc != expectedRc {
		return "", fmt.Errorf("expected a %d response, got %d", expectedRc, rc)
	}
	if rc != 200 {
		return "", nil
	}

	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("got an error reading the body: %v", err)
	}
	results := string(body)
	if !strings.Contains(results, result) {
		//log.Printf("\n\nRESULTS: %s\n\n", results)
		return results, fmt.Errorf("response did not have NATS data")
	}
	return results, nil
}

func checkExporter(t *testing.T, addr string, secure bool) error {
	_, err := checkExporterFull(t, "", "", addr, "nats_core_cpu", "/metrics", secure, http.StatusOK)
	return err
}

func checkExporterForResult(t *testing.T, addr, result string, secure bool) (string, error) {
	return checkExporterFull(t, "", "", addr, result, "/metrics", secure, http.StatusOK)
}

func TestSurveyor_Basic(t *testing.T) {
	sc := NewSuperCluster(t)
	defer sc.Shutdown()
	opts := GetDefaultOptions()
	opts.Credentials = "../test/SYS.creds"
	s, err := NewSurveyor(opts)
	if err != nil {
		t.Fatalf("couldn't create surveyor: %v", err)
	}
	if err = s.Start(); err != nil {
		t.Fatalf("start error: %v", err)
	}
	if err := checkExporter(t, "127.0.0.1:7777", false); err != nil {
		t.Fatalf("couldn't poll exporter:  %v\n", err)
	}
	s.Stop()
}
