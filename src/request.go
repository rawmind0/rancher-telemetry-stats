package main

import (
	"fmt"
	//"regexp"
	"bytes"
	"encoding/json"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/Sirupsen/logrus"
	influx "github.com/influxdata/influxdb/client/v2"
	"github.com/oschwald/maxminddb-golang"
	"github.com/urfave/cli"
)

func getJSON(url string, accessKey string, secretKey string, target interface{}) error {

	start := time.Now()

	log.Info("Connecting to ", url)

	client := &http.Client{}
	req, err := http.NewRequest("GET", url, nil)
	req.SetBasicAuth(accessKey, secretKey)
	resp, err := client.Do(req)

	if err != nil {
		log.Error("Error Collecting JSON from API: ", err)
		panic(err)
	}

	respFormatted := json.NewDecoder(resp.Body).Decode(target)

	// Timings recorded as part of internal metrics
	log.Info("Time to get json: ", float64((time.Since(start))/time.Millisecond), " ms")

	// Close the response body, the underlying Transport should then close the connection.
	resp.Body.Close()

	// return formatted JSON
	return respFormatted
}

type reqLocation struct {
	City    string `json:"city"`
	Country struct {
		Name    string
		ISOCode string
	} `json:"country"`
}

type Request struct {
	Id        int64                  `json:"id"`
	Uid       string                 `json:"uid"`
	FirstSeen time.Time              `json:"first_seen"`
	LastSeen  time.Time              `json:"last_seen"`
	LastIp    string                 `json:"last_ip"`
	RecordVer string                 `json:"record_version"`
	Record    map[string]interface{} `json:"record"`
	Location  reqLocation            `json:"location"`
	Status    string                 `json:"status"`
}

func newrequestByString(line, sep string) *Request {
	var err error

	s := strings.Split(line, sep)
	if len(s) == 6 {
		req := &Request{}

		req.Id, err = strconv.ParseInt(s[0], 10, 64)
		if err != nil {
			log.Info("Error parsing Id ", s[0], err)
		}
		req.Uid = s[1]
		req.FirstSeen, err = time.Parse("2006-01-02 15:04:05.999999", s[2])
		if err != nil {
			log.Info("Error parsing Firstseen ", s[2], err)
		}
		req.LastSeen, err = time.Parse("2006-01-02 15:04:05.999999", s[3])
		if err != nil {
			log.Info("Error parsing Lastseen ", s[3], err)
		}
		req.LastIp = s[4]
		err = json.Unmarshal([]byte(s[5]), &req.Record)
		if err != nil {
			log.Info("Error unmarshaling Record ", s[5], err)
		}

		return req
	}
	return nil
}

func (r *Request) checkData() bool {
	if len(r.Uid) < 3 || r.Record == nil {
		return false
	}

	r.RecordVer = strconv.FormatFloat(r.Record["r"].(float64), 'f', 0, 64)

	var record_key []string

	if r.RecordVer == "1" {
		record_key = []string{"container", "environment", "host", "service", "stack", "install"}
	}

	if r.RecordVer == "2" {
		record_key = []string{"cluster", "node", "project", "install"}
	}

	for _, key := range record_key {
		if r.Record[key] == nil {
			return false
		}
	}

	return true
}

// Produce wire-formatted string for ingestion into influxdb
func (r *Request) printInflux() {
	p := r.getPoint()
	fmt.Println(p.String())
}

func (r *Request) getPoint() *influx.Point {
	var n = "telemetry"
	var v map[string]interface{}
	var t map[string]string

	if r.RecordVer == "1" {
		orch := r.Record["environment"].(map[string]interface{})["orch"].(map[string]interface{})
		orch_key := [5]string{"cattle", "kubernetes", "mesos", "swarm", "windows"}
		for _, key := range orch_key {
			if orch[key] == nil {
				orch[key] = float64(0)
			}
		}
		v = map[string]interface{}{
			"ip":                   r.LastIp,
			"uid":                  r.Uid,
			"container_running":    r.Record["container"].(map[string]interface{})["running"],
			"container_total":      r.Record["container"].(map[string]interface{})["total"],
			"environment_total":    r.Record["environment"].(map[string]interface{})["total"],
			"orch_cattle":          orch["cattle"],
			"orch_kubernetes":      orch["kubernetes"],
			"orch_mesos":           orch["mesos"],
			"orch_swarm":           orch["swarm"],
			"orch_windows":         orch["windows"],
			"host_active":          r.Record["host"].(map[string]interface{})["active"],
			"host_cpu_cores_total": r.Record["host"].(map[string]interface{})["cpu"].(map[string]interface{})["cores_total"],
			"host_mem_mb_total":    r.Record["host"].(map[string]interface{})["mem"].(map[string]interface{})["mb_total"],
			"service_active":       r.Record["service"].(map[string]interface{})["active"],
			"service_total":        r.Record["service"].(map[string]interface{})["total"],
			"stack_active":         r.Record["stack"].(map[string]interface{})["active"],
			"stack_total":          r.Record["stack"].(map[string]interface{})["total"],
			"stack_from_catalog":   r.Record["stack"].(map[string]interface{})["from_catalog"],
		}
	}
	if r.RecordVer == "2" {
		v = map[string]interface{}{
			"ip":                             r.LastIp,
			"uid":                            r.Uid,
			"cluster_active":                 r.Record["cluster"].(map[string]interface{})["active"],
			"cluster_total":                  r.Record["cluster"].(map[string]interface{})["total"],
			"cluster_namespace_total":        r.Record["cluster"].(map[string]interface{})["namespace"].(map[string]interface{})["total"],
			"cluster_namespace_from_catalog": r.Record["cluster"].(map[string]interface{})["namespace"].(map[string]interface{})["from_catalog"],
			"cluster_cpu_total":              r.Record["cluster"].(map[string]interface{})["cpu"].(map[string]interface{})["cores_total"],
			"cluster_cpu_util":               r.Record["cluster"].(map[string]interface{})["cpu"].(map[string]interface{})["util_avg"],
			"cluster_mem_mb_total":           r.Record["cluster"].(map[string]interface{})["mem"].(map[string]interface{})["mb_total"],
			"cluster_mem_util":               r.Record["cluster"].(map[string]interface{})["mem"].(map[string]interface{})["util_avg"],
			"node_active":                    r.Record["node"].(map[string]interface{})["active"],
			"node_total":                     r.Record["node"].(map[string]interface{})["total"],
			"node_from_template":             r.Record["node"].(map[string]interface{})["from_template"],
			"node_mem_mb_total":              r.Record["node"].(map[string]interface{})["mem"].(map[string]interface{})["mb_total"],
			"node_mem_util":                  r.Record["node"].(map[string]interface{})["mem"].(map[string]interface{})["util_avg"],
			"node_role_controlplane":         r.Record["node"].(map[string]interface{})["role"].(map[string]interface{})["controlplane"],
			"node_role_etcd":                 r.Record["node"].(map[string]interface{})["role"].(map[string]interface{})["etcd"],
			"node_role_worker":               r.Record["node"].(map[string]interface{})["role"].(map[string]interface{})["worker"],
			"project_total":                  r.Record["project"].(map[string]interface{})["total"],
			"project_namespace_total":        r.Record["project"].(map[string]interface{})["namespace"].(map[string]interface{})["total"],
			"project_namespace_from_catalog": r.Record["project"].(map[string]interface{})["namespace"].(map[string]interface{})["from_catalog"],
			"project_workload_total":         r.Record["project"].(map[string]interface{})["workload"].(map[string]interface{})["total"],
			"project_pod_total":              r.Record["project"].(map[string]interface{})["pod"].(map[string]interface{})["total"],
		}
	}

	t = map[string]string{
		"id":              strconv.FormatInt(r.Id, 10),
		"uid":             r.Uid,
		"ip":              r.LastIp,
		"install_image":   r.Record["install"].(map[string]interface{})["image"].(string),
		"install_version": r.Record["install"].(map[string]interface{})["version"].(string),
		"record_version":  r.RecordVer,
		"city":            r.Location.City,
		"country":         r.Location.Country.Name,
		"country_isocode": r.Location.Country.ISOCode,
		"status":          r.Status,
	}

	m, err := influx.NewPoint(n, t, v, r.LastSeen)
	if err != nil {
		log.Warn(err)
	}

	return m
}

func (r *Request) Marshal() ([]byte, error) {
	return json.Marshal(r)
}

func (r *Request) printJson() {

	j, err := json.Marshal(r)
	if err != nil {
		log.Error("json", err)
	}

	fmt.Println(string(j))

}

func (r *Request) getLocation(geoipdb string) {
	db, err := maxminddb.Open(geoipdb)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()

	ip := net.ParseIP(r.LastIp)

	var record struct {
		City struct {
			Names map[string]string `maxminddb:"names"`
		} `maxminddb:"city"`
		Country struct {
			Names   map[string]string `maxminddb:"names"`
			ISOCode string            `maxminddb:"iso_code"`
		} `maxminddb:"country"`
	} // Or any appropriate struct

	err = db.Lookup(ip, &record)
	if err != nil {
		log.Fatal(err)
	}

	r.Location.City = record.City.Names["en"]
	r.Location.Country.Name = record.Country.Names["en"]
	r.Location.Country.ISOCode = record.Country.ISOCode

}

func (r *Request) isNew() string {
	diff := r.LastSeen.Sub(r.FirstSeen)

	if diff.Hours() > 24 {
		return "active"
	} else {
		return "new"
	}
}

func (r *Request) getData(geoipdb string) bool {
	if !r.checkData() {
		log.Debugf("Skipping request without correct data")
		return false
	}

	now := time.Now()
	diff := now.Sub(r.LastSeen)

	if diff < 0 {
		log.Debugf("Skipping request with time in future ", r.LastSeen)
		r.printJson()
		return false
	}

	r.getLocation(geoipdb)
	r.Status = r.isNew()

	return true

}

type Params struct {
	url        string
	accessKey  string
	secretKey  string
	influxurl  string
	influxdb   string
	influxuser string
	influxpass string
	geoipdb    string
	file       string
	format     string
	preview    bool
	limit      int
	refresh    int
	flush      int
}

func RunRequests(c *cli.Context) {
	params := Params{
		url:        c.String("url"),
		accessKey:  c.String("accesskey"),
		secretKey:  c.String("secretkey"),
		influxurl:  c.String("influxurl"),
		influxdb:   c.String("influxdb"),
		influxuser: c.String("influxuser"),
		influxpass: c.String("influxpass"),
		geoipdb:    c.String("geoipdb"),
		file:       c.String("file"),
		format:     c.String("format"),
		preview:    c.Bool("preview"),
		limit:      c.Int("limit"),
		refresh:    c.Int("refresh"),
		flush:      c.Int("flush"),
	}
	req := newRequests(params)
	req.getDataByUrl()
}

type Requests struct {
	Input   chan *Request
	Output  chan *influx.Point
	Exit    chan os.Signal
	Readers []chan struct{}
	Config  Params
	Data    []Request `json:"data"`
}

func newRequests(conf Params) *Requests {
	var r = &Requests{
		Readers: []chan struct{}{},
		Config:  conf,
	}

	r.Input = make(chan *Request, 1)
	r.Output = make(chan *influx.Point, 1)
	r.Exit = make(chan os.Signal, 1)
	signal.Notify(r.Exit, os.Interrupt, os.Kill)

	customFormatter := new(log.TextFormatter)
	customFormatter.TimestampFormat = "2006-01-02 15:04:05"
	log.SetFormatter(customFormatter)
	customFormatter.FullTimestamp = true

	return r
}

func (r *Requests) Close() {
	close(r.Input)
	close(r.Output)
	close(r.Exit)

}

func (r *Requests) sendToInflux() {
	var points []influx.Point
	var index, p_len int

	i := newInflux(r.Config.influxurl, r.Config.influxdb, r.Config.influxuser, r.Config.influxpass)

	if i.Connect() {
		connected := i.CheckConnect(r.Config.refresh)
		defer i.Close()

		ticker := time.NewTicker(time.Second * time.Duration(r.Config.flush))

		index = 0
		for {
			select {
			case <-connected:
				return
			case <-ticker.C:
				if len(points) > 0 {
					log.Info("Tick: Sending to influx ", len(points), " points")
					if i.sendToInflux(points, 1) {
						points = []influx.Point{}
					} else {
						return
					}
				}
			case p := <-r.Output:
				if p != nil {
					points = append(points, *p)
					p_len = len(points)
					if p_len == r.Config.limit {
						log.Info("Batch: Sending to influx ", p_len, " points")
						if i.sendToInflux(points, 1) {
							points = []influx.Point{}
						} else {
							return
						}
					}
					index++
				} else {
					p_len = len(points)
					if p_len > 0 {
						log.Info("Batch: Sending to influx ", p_len, " points")
						if i.sendToInflux(points, 1) {
							points = []influx.Point{}
						}
					}
					return
				}
			}
		}
	}
}

func (r *Requests) addReader() chan struct{} {
	chan_new := make(chan struct{}, 1)
	r.Readers = append(r.Readers, chan_new)

	return chan_new
}

func (r *Requests) closeReaders() {
	for _, r_chan := range r.Readers {
		if r_chan != nil {
			r_chan <- struct{}{}
		}
	}
	r.Readers = nil
}

func (r *Requests) getDataByUrl() {
	var in, out sync.WaitGroup
	indone := make(chan struct{}, 1)
	outdone := make(chan struct{}, 1)

	in.Add(1)
	go func() {
		defer in.Done()
		if r.Config.file != "" {
			r.getDataByFile()
		} else {
			r.getDataByChan(r.addReader())
		}
	}()

	out.Add(1)
	go func() {
		defer out.Done()
		r.getOutput()
	}()

	go func() {
		in.Wait()
		close(r.Input)
		close(r.Output)
		close(indone)
	}()

	go func() {
		out.Wait()
		close(outdone)
	}()

	for {
		select {
		case <-indone:
			<-outdone
			return
		case <-outdone:
			log.Error("Aborting...")
			go r.closeReaders()
			return
		case <-r.Exit:
			//close(r.Exit)
			log.Info("Exit signal detected....Closing...")
			go r.closeReaders()
			select {
			case <-outdone:
				return
			}
		}
	}
}

func (r *Requests) getDataByChan(stop chan struct{}) {

	ticker := time.NewTicker(time.Second * time.Duration(r.Config.refresh))

	go r.getData()

	for {
		select {
		case <-ticker.C:
			log.Info("Tick: Getting data...")
			go r.getData()
		case <-stop:
			return
		}
	}
}

func (r *Requests) getDataByFile() {
	log.Info("Reading file ", r.Config.file)
	dat, err := ioutil.ReadFile(r.Config.file)

	if err != nil {
		log.Error("Error reading file ", r.Config.file, err)
	}

	lines := bytes.Split(dat, []byte("\n"))

	for _, line := range lines {
		if line != nil {
			req := newrequestByString(string(line), "|")
			if req != nil {
				r.Data = append(r.Data, *req)
			}
		}
	}

	for _, req := range r.Data {
		aux := Request{}
		aux = req
		if aux.getData(r.Config.geoipdb) {
			if r.Config.format == "json" {
				r.Input <- &aux
			} else {
				r.Output <- aux.getPoint()
			}
		}
	}
}

func (r *Requests) getData() {
	var uri string

	uri = "/admin/active"

	err := getJSON(r.Config.url+uri, r.Config.accessKey, r.Config.secretKey, r)
	if err != nil {
		log.Error("Error getting JSON from URL ", r.Config.url+uri, err)
	}

	for _, req := range r.Data {
		aux := Request{}
		aux = req
		if aux.getData(r.Config.geoipdb) {
			if r.Config.format == "json" {
				r.Input <- &aux
			} else {
				r.Output <- aux.getPoint()
			}
		}
	}
}

func (r *Requests) print() {
	if r.Config.format == "json" {
		r.printJson()
	} else {
		r.printInflux()
	}
}

func (r *Requests) getOutput() {
	if r.Config.format == "json" {
		r.printJson()
	} else {
		if r.Config.preview {
			r.printInflux()
		} else {
			r.sendToInflux()
		}
	}
}

func (r *Requests) printJson() {
	for {
		select {
		case req := <-r.Input:
			if req != nil {
				req.printJson()
			} else {
				return
			}
		}
	}
}

func (r *Requests) printInflux() {
	for {
		select {
		case p := <-r.Output:
			if p != nil {
				fmt.Println(p.String())
			} else {
				return
			}
		}
	}
}
