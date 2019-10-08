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
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"sync"
	"time"

	nats "github.com/nats-io/go-nats"
	server "github.com/nats-io/nats-server/server"
	"github.com/prometheus/client_golang/prometheus"
)

// statzDescs holds the metric descriptions
type statzDescs struct {
	Info             *prometheus.Desc
	Start            *prometheus.Desc
	Mem              *prometheus.Desc
	Cores            *prometheus.Desc
	CPU              *prometheus.Desc
	Connections      *prometheus.Desc
	TotalConnections *prometheus.Desc
	ActiveAccounts   *prometheus.Desc
	NumSubs          *prometheus.Desc
	SentMsgs         *prometheus.Desc
	SentBytes        *prometheus.Desc
	ReceivedMsgs     *prometheus.Desc
	ReceivedBytes    *prometheus.Desc
	SlowConsumers    *prometheus.Desc
	//Routes           []*RouteStat   `json:"routes,omitempty"`
	//Gateways         []*GatewayStat `json:"gateways,omitempty"`
}

// StatzCollector collects statz from a server deployment
type StatzCollector struct {
	sync.Mutex
	nc          *nats.Conn
	start       time.Time
	stats       []*server.ServerStatsMsg
	rtts        map[string]time.Duration
	pollTimeout time.Duration
	reply       string
	polling     bool
	numServers  int
	more        int
	servers     map[string]bool
	doneCh      chan struct{}
	moreCh      chan struct{}
	descs       statzDescs
}

var (
	serverlabels = []string{"nats_server_cluster", "nats_server_host", "nats_server_id"}
)

func getLabelValues(stats *server.ServerStatsMsg) []string {
	return []string{stats.Server.Cluster, stats.Server.Host, stats.Server.ID}
}

func newServerDesc(name, help string) *prometheus.Desc {
	return prometheus.NewDesc(
		prometheus.BuildFQName("nats", "core", name), help, serverlabels, nil)
}

func buildDescs(sc *StatzCollector) {
	sc.descs.Start = newServerDesc("start", "Server start time")
	sc.descs.Mem = newServerDesc("mem", "Server memory")
	sc.descs.Cores = newServerDesc("cores", "Machine cores")
	sc.descs.CPU = newServerDesc("cpu", "Server cpu utilization")
	sc.descs.Connections = newServerDesc("connections", "Current number of client connections")
	sc.descs.TotalConnections = newServerDesc("totalconns", "Total number of client connections serviced")
	sc.descs.ActiveAccounts = newServerDesc("activeaccounts", "Number of active accounts")
	sc.descs.NumSubs = newServerDesc("subs", "Current number of subscriptions")
	sc.descs.SentMsgs = newServerDesc("sentmsgs", "Number of messages sent")
	sc.descs.SentBytes = newServerDesc("sentbytes", "Number of messages sent")
	sc.descs.ReceivedMsgs = newServerDesc("recvdmsgs", "Number of messages received")
	sc.descs.ReceivedBytes = newServerDesc("recvdbytes", "Number of messages received")
	sc.descs.SlowConsumers = newServerDesc("slowconsumers", "Number of slow consumers")

	// TODO Routez
	// TODO Gatewayz
}

// NewCollector creates a
func NewCollector(nc *nats.Conn, numServers int, pollTimeout time.Duration) *StatzCollector {
	sc := &StatzCollector{
		nc:          nc,
		numServers:  numServers,
		reply:       nc.NewRespInbox(),
		pollTimeout: pollTimeout,
		//stats : make()
		servers: make(map[string]bool, numServers),
		doneCh:  make(chan struct{}, 1),
		moreCh:  make(chan struct{}, 1),
	}
	buildDescs(sc)
	nc.Subscribe(sc.reply, sc.handleResponse)
	return sc
}

func (sc *StatzCollector) handleResponse(msg *nats.Msg) {
	m := &server.ServerStatsMsg{}
	if err := json.Unmarshal(msg.Data, m); err != nil {
		log.Printf("Error unmarshalling statz json: %v", err)
	}

	sc.Lock()
	rtt := time.Now().Sub(sc.start)
	if sc.polling {
		sc.stats = append(sc.stats, m)
		if len(sc.stats) == sc.numServers {
			sc.polling = false
			sc.doneCh <- struct{}{}
		}
	} else if len(sc.stats) < sc.numServers {
		log.Printf("Late reply for server [%15s : %15s : %s]: %v", m.Server.Cluster, m.Server.Host, m.Server.ID, rtt)
	} else {
		log.Printf("Extra reply from server [%15s : %15s : %s]: %v", m.Server.Cluster, m.Server.Host, m.Server.ID, rtt)
		sc.more++
		if sc.more == 1 {
			sc.moreCh <- struct{}{}
		}
	}
	sc.Unlock()
}

func (sc *StatzCollector) poll() {
	sc.Lock()
	sc.start = time.Now()
	sc.polling = true
	sc.stats = nil
	sc.rtts = make(map[string]time.Duration, sc.numServers)
	sc.more = 0
	// drain possible notification from previous poll
	select {
	case <-sc.moreCh:
	default:
	}
	sc.Unlock()

	// Send our ping for statusz updates
	log.Printf("Sending Request")
	if err := sc.nc.PublishRequest("$SYS.REQ.SERVER.PING", sc.reply, nil); err != nil {
		log.Fatal(err)
	}

	// Wait to collect all the servers responses.
	select {
	case <-sc.doneCh:
	case <-time.After(sc.pollTimeout):
	}

	sc.Lock()
	sc.polling = false
	ns := len(sc.stats)
	stats := append([]*server.ServerStatsMsg(nil), sc.stats...)
	rtts := sc.rtts
	sc.Unlock()

	// If we do not see expected number of servers complain.
	if ns != sc.numServers {
		sort.Slice(stats, func(i, j int) bool {
			a := fmt.Sprintf("%s-%s", stats[i].Server.Cluster, stats[i].Server.Host)
			b := fmt.Sprintf("%s-%s", stats[j].Server.Cluster, stats[j].Server.Host)
			return a < b
		})

		// Reset the state of what server has been seen
		for key := range sc.servers {
			sc.servers[key] = false
		}

		log.Println("RTTs for responding servers:")
		for _, stat := range stats {
			// We use for key the cluster name followed by IP (Host)
			key := fmt.Sprintf("%s:%s", stat.Server.Cluster, stat.Server.Host)
			// Mark this server has been seen
			sc.servers[key] = true
			log.Printf("Server [%15s : %15s : %s]: %v\n", stat.Server.Cluster, stat.Server.Host, stat.Server.ID, rtts[stat.Server.ID])
		}

		log.Println("Missing servers:")
		missingServers := []string{}
		for key, seen := range sc.servers {
			if !seen {
				log.Println(key)
				missingServers = append(missingServers, "["+key+"]")
			}
		}
		log.Printf("Expected %d servers, only saw responses from %d. Missing %v", sc.numServers, ns, missingServers)
	}

	if ns == sc.numServers {
		// Build map of what is our expected set...
		sc.servers = make(map[string]bool, sc.numServers)
		for _, stat := range stats {
			key := fmt.Sprintf("%s:%s", stat.Server.Cluster, stat.Server.Host)
			sc.servers[key] = false
		}
	}
}

// Describe is the Prometheus interface to describe metrics for
// the prometheus system
func (sc *StatzCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- sc.descs.Start
	ch <- sc.descs.Mem
	ch <- sc.descs.Cores
	ch <- sc.descs.CPU
	ch <- sc.descs.Connections
	ch <- sc.descs.TotalConnections
	ch <- sc.descs.ActiveAccounts
	ch <- sc.descs.NumSubs
	ch <- sc.descs.SentMsgs
	ch <- sc.descs.SentBytes
	ch <- sc.descs.ReceivedMsgs
	ch <- sc.descs.ReceivedBytes
	ch <- sc.descs.SlowConsumers

	// TODO Routez
	// TODO Channelz
}

func getGaugeMetric(sm *server.ServerStatsMsg, desc *prometheus.Desc, value float64) prometheus.Metric {
	return prometheus.MustNewConstMetric(desc, prometheus.GaugeValue, value, getLabelValues(sm)...)
}

// Collect gathers the streaming server serverz metrics.
func (sc *StatzCollector) Collect(ch chan<- prometheus.Metric) {

	// poll the servers
	sc.poll()

	sc.Lock()
	defer sc.Unlock()

	for _, sm := range sc.stats {
		ch <- getGaugeMetric(sm, sc.descs.Start, float64(sm.Stats.Start.UnixNano()))
		ch <- getGaugeMetric(sm, sc.descs.Mem, float64(sm.Stats.Mem))
		ch <- getGaugeMetric(sm, sc.descs.Cores, float64(sm.Stats.Cores))
		ch <- getGaugeMetric(sm, sc.descs.CPU, float64(sm.Stats.CPU))
		ch <- getGaugeMetric(sm, sc.descs.Connections, float64(sm.Stats.Connections))
		ch <- getGaugeMetric(sm, sc.descs.TotalConnections, float64(sm.Stats.TotalConnections))
		ch <- getGaugeMetric(sm, sc.descs.ActiveAccounts, float64(sm.Stats.ActiveAccounts))
		ch <- getGaugeMetric(sm, sc.descs.NumSubs, float64(sm.Stats.NumSubs))
		ch <- getGaugeMetric(sm, sc.descs.SentMsgs, float64(sm.Stats.Sent.Msgs))
		ch <- getGaugeMetric(sm, sc.descs.SentBytes, float64(sm.Stats.Sent.Bytes))
		ch <- getGaugeMetric(sm, sc.descs.ReceivedMsgs, float64(sm.Stats.Received.Msgs))
		ch <- getGaugeMetric(sm, sc.descs.ReceivedBytes, float64(sm.Stats.Received.Bytes))
		ch <- getGaugeMetric(sm, sc.descs.SlowConsumers, float64(sm.Stats.SlowConsumers))

		/*
			 * TODO - gateways and routes
			for _, gw := range sm.Stats.Gateways {
				gw.ID
				gw.Name
				gw.Sent.Msgs
				gw.Sent.Bytes
				gw.Received.Msgs
				gw.Received.Bytes
				gw.NumInbound
			}

			for _, r := range sm.Stats.Routes {
				r.ID
				r.Name
				r.Sent.Msgs
				r.Sent.Bytes
				r.Received.Msgs
				r.Received.Bytes
				r.Pending
			}
		*/
	}

	// SEND Request
	// WAIT
	// Process Response
	/*
		for _, server := range nc.stats {
			var resp replicatorVarz
			if err := getMetricURL(nc.httpClient, server.URL, &resp); err != nil {
				Debugf("ignoring server %s: %v\n", server.ID, err)
				continue
			}

			ch <- prometheus.MustNewConstMetric(nc.requestCount, prometheus.CounterValue, float64(resp.RequestCount), server.ID)
			ch <- prometheus.MustNewConstMetric(nc.startTime, prometheus.CounterValue, float64(resp.StartTime), server.ID)
			ch <- prometheus.MustNewConstMetric(nc.currentTime, prometheus.CounterValue, float64(resp.CurrentTime), server.ID)
			ch <- prometheus.MustNewConstMetric(nc.info, prometheus.GaugeValue, 1, server.ID, resp.Uptime)
			for _, c := range resp.Connectors {

				labelValues := []string{server.ID, c.ID, c.Name}

				connected := float64(0)
				if c.Connected == true {
					connected = float64(1)
				}
				ch <- prometheus.MustNewConstMetric(nc.connected, prometheus.GaugeValue, connected, labelValues...)
				ch <- prometheus.MustNewConstMetric(nc.connects, prometheus.GaugeValue, float64(c.Connects), labelValues...)
				ch <- prometheus.MustNewConstMetric(nc.disconnects, prometheus.GaugeValue, float64(c.Disconnects), labelValues...)
				ch <- prometheus.MustNewConstMetric(nc.bytesIn, prometheus.GaugeValue, float64(c.BytesIn), labelValues...)
				ch <- prometheus.MustNewConstMetric(nc.bytesOut, prometheus.GaugeValue, float64(c.BytesOut), labelValues...)
				ch <- prometheus.MustNewConstMetric(nc.messagesIn, prometheus.GaugeValue, float64(c.MessagesIn), labelValues...)
				ch <- prometheus.MustNewConstMetric(nc.messagesOut, prometheus.GaugeValue, float64(c.MessagesOut), labelValues...)
				ch <- prometheus.MustNewConstMetric(nc.count, prometheus.GaugeValue, float64(c.RequestCount), labelValues...)
				ch <- prometheus.MustNewConstMetric(nc.movingAverage, prometheus.GaugeValue, float64(c.MovingAverage), labelValues...)
				ch <- prometheus.MustNewConstMetric(nc.quintile50, prometheus.GaugeValue, float64(c.Quintile50), labelValues...)
				ch <- prometheus.MustNewConstMetric(nc.quintile75, prometheus.GaugeValue, float64(c.Quintile75), labelValues...)
				ch <- prometheus.MustNewConstMetric(nc.quintile90, prometheus.GaugeValue, float64(c.Quintile90), labelValues...)
				ch <- prometheus.MustNewConstMetric(nc.quintile95, prometheus.GaugeValue, float64(c.Quintile95), labelValues...)
			}
		}
	*/
}
