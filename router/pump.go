package router

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/fsouza/go-dockerclient"
)

var topic_brokers_map = make(map[string]string)

type ClusterData struct {
	Brokers    []string `json:"brokers"`
	Cluster    string   `json:"cluster"`
	Zookeepers string   `json:"zookeepers"`
}

type TopicData struct {
	Zookeepers string `json:"zookeepers"`
	Topic      string `json:"topic"`
	Cluster    string `json:"cluster"`
}

var time_regexp_map = make(map[string]*regexp.Regexp)

const consul_api_prefix = "http://localhost:8500/v1/kv/"
const franz_api_prefix = "http://localhost:8500/v1/kv/franz/cluster/"

func get_broker_list_from_consul(app string, topic string) (broker_list string, err error) {
	var cluster ClusterData
	var topicdata TopicData
	consul_request_url := fmt.Sprintf("%s%s/kafka/%s?raw", consul_api_prefix, app, topic)
	log.Println(consul_request_url)
	err = call_api("GET", consul_request_url, []byte{}, &topicdata)
	if err != nil {
		log.Println("call consul api failed:", err)
		return "localhost:9092", err
	}
	franz_request_url := fmt.Sprintf("%s%s?raw", franz_api_prefix, topicdata.Cluster)
	log.Println(franz_request_url)
	err = call_api("GET", franz_request_url, []byte{}, &cluster)
	if err != nil {
		log.Println("call franz api failed:", err)
		return "localhost:9092", err
	}
	return strings.Join(cluster.Brokers, ","), nil
}

func call_api(method string, request_url string, params interface{}, response interface{}) error {
	var req *http.Request
	var err error
	//request_url := fmt.Sprintf("%s%s?raw", halo_api_prefix, url)
	if method == "GET" {
		req, err = http.NewRequest("GET", request_url, nil)
	} else {
		data := []byte{}
		if params != nil {
			data, _ = json.Marshal(params)
		}
		req, err = http.NewRequest(method, request_url, bytes.NewBuffer(data))
	}
	if err != nil {
		return err
	}
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if response != nil {
		err = json.Unmarshal(body, response)
	}
	return err
}

func init() {
	//time_regexp = regexp.MustCompile(`^(\d{4})[-,/](\d{2})[-,/](\d{2})`)
	pump := &LogsPump{
		pumps:  make(map[string]*containerPump),
		routes: make(map[chan *update]struct{}),
	}
	LogRouters.Register(pump, "pump")
	Jobs.Register(pump, "pump")
}

func getopt(name, dfault string) string {
	value := os.Getenv(name)
	if value == "" {
		value = dfault
	}
	return value
}

func debug(v ...interface{}) {
	if os.Getenv("DEBUG") != "" {
		log.Println(v...)
	}
}

func assert(err error, context string) {
	if err != nil {
		log.Fatal(context+": ", err)
	}
}

func normalName(name string) string {
	return name[1:]
}

func normalID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func logDriverSupported(container *docker.Container) bool {
	switch container.HostConfig.LogConfig.Type {
	case "json-file", "journald":
		return true
	default:
		return false
	}
}

func ignoreContainer(container *docker.Container) bool {
	for _, kv := range container.Config.Env {
		kvp := strings.SplitN(kv, "=", 2)
		if len(kvp) == 2 && kvp[0] == "LOGSPOUT" && strings.ToLower(kvp[1]) == "ignore" {
			return true
		}
	}
	excludeLabel := getopt("EXCLUDE_LABEL", "")
	if value, ok := container.Config.Labels[excludeLabel]; ok {
		return len(excludeLabel) > 0 && strings.ToLower(value) == "true"
	}
	return false
}

type update struct {
	*docker.APIEvents
	pump *containerPump
}

type LogsPump struct {
	mu     sync.Mutex
	pumps  map[string]*containerPump
	routes map[chan *update]struct{}
	client *docker.Client
}

func (p *LogsPump) Name() string {
	return "pump"
}

func (p *LogsPump) Setup() error {
	var err error
	p.client, err = docker.NewClientFromEnv()
	return err
}

func (p *LogsPump) rename(event *docker.APIEvents) {
	p.mu.Lock()
	defer p.mu.Unlock()
	container, err := p.client.InspectContainer(event.ID)
	assert(err, "pump")
	pump, ok := p.pumps[normalID(event.ID)]
	if !ok {
		debug("pump.rename(): ignore: pump not found, state:", container.State.StateString())
		return
	}
	pump.container.Name = container.Name
}

func (p *LogsPump) Run() error {
	containers, err := p.client.ListContainers(docker.ListContainersOptions{})
	if err != nil {
		return err
	}
	for _, listing := range containers {
		p.pumpLogs(&docker.APIEvents{
			ID:     normalID(listing.ID),
			Status: "start",
		}, false)
	}
	events := make(chan *docker.APIEvents)
	err = p.client.AddEventListener(events)
	if err != nil {
		return err
	}
	for event := range events {
		debug("pump.Run() event:", normalID(event.ID), event.Status)
		switch event.Status {
		case "start", "restart":
			go p.pumpLogs(event, true)
		case "rename":
			go p.rename(event)
		case "die":
			go p.update(event)
		}
	}
	return errors.New("docker event stream closed")
}

func (p *LogsPump) pumpLogs(event *docker.APIEvents, backlog bool) {
	var kafka_addr string
	id := normalID(event.ID)
	container, err := p.client.InspectContainer(id)
	assert(err, "pump")
	//add route

	filter := getopt("FILTER", "TOPIC")
	topic_env := getEnv(filter, container.Config.Env, "default")
	if topic_env == "default" {
		return
	}
	app := getEnv("ZHIHU_APP_NAME", container.Config.Env, "app_name")
	kafka_addr, ok := topic_brokers_map[topic_env]
	if !ok {
		kafka_addr, err = get_broker_list_from_consul(app, topic_env)
		if err != nil {
			kafka_addr = getopt("KAFKA", "localhost:9092")
		} else {
			topic_brokers_map[topic_env] = kafka_addr
		}
	}
	if _, ok := time_regexp_map[topic_env]; !ok {
		regex_env := getEnv("LOG_TIME_REGEX", container.Config.Env, "^\\[[A-Z]|^(\\d{4})[-,/](\\d{2})[-,/](\\d{2})")
		time_regexp_map[topic_env], err = regexp.Compile(regex_env)
		if err != nil {
			log.Fatal("LOG_TIME_REGEX set wrong:", err)
			return
		}
	}
	if kafka_addr != "" && topic_env != "default" {
		route := &Route{
			ID:        topic_env,
			Adapter:   "kafka",
			FilterEnv: topic_env,
			Address:   kafka_addr + "/" + topic_env,
		}
		Routes.Add(route)
		log.Println("add route kafka topic:", kafka_addr, topic_env)
	}
	if container.Config.Tty {
		debug("pump.pumpLogs():", id, "ignored: tty enabled")
		return
	}
	if ignoreContainer(container) {
		debug("pump.pumpLogs():", id, "ignored: environ ignore")
		return
	}
	if !logDriverSupported(container) {
		debug("pump.pumpLogs():", id, "ignored: log driver not supported")
		return
	}

	var sinceTime time.Time
	if backlog {
		sinceTime = time.Unix(0, 0)
	} else {
		str_sinceTime := os.Getenv("SINCE_TIME") //first read sincetime from file via env
		int_sinceTime, _ := strconv.ParseInt(str_sinceTime, 10, 64)
		sinceTime = time.Unix(int_sinceTime, 0)
	}

	p.mu.Lock()
	if _, exists := p.pumps[id]; exists {
		p.mu.Unlock()
		debug("pump.pumpLogs():", id, "pump exists")
		return
	}
	outrd, outwr := io.Pipe()
	errrd, errwr := io.Pipe()
	//containerPump use to recieve log from pump
	p.pumps[id] = newContainerPump(container, outrd, errrd)
	p.mu.Unlock()
	p.update(event)
	go func() {
		for {
			debug("pump.pumpLogs():", id, "started")
			err := p.client.Logs(docker.LogsOptions{
				Container:    id,
				OutputStream: outwr,
				ErrorStream:  errwr,
				Stdout:       true,
				Stderr:       true,
				Follow:       true,
				Tail:         "all",
				Since:        sinceTime.Unix(),
			})
			if err != nil {
				debug("pump.pumpLogs():", id, "stopped with error:", err)
			} else {
				debug("pump.pumpLogs():", id, "stopped")
			}

			sinceTime = time.Now()
			container, err := p.client.InspectContainer(id)
			if err != nil {
				_, four04 := err.(*docker.NoSuchContainer)
				if !four04 {
					assert(err, "pump")
				}
			} else if container.State.Running {
				continue
			}

			debug("pump.pumpLogs():", id, "dead")
			outwr.Close()
			errwr.Close()
			p.mu.Lock()
			delete(p.pumps, id)
			p.mu.Unlock()
			return
		}
	}()
}

func (p *LogsPump) update(event *docker.APIEvents) {
	p.mu.Lock()
	defer p.mu.Unlock()
	pump, pumping := p.pumps[normalID(event.ID)]
	if pumping {
		for r := range p.routes {
			select {
			case r <- &update{event, pump}:
			case <-time.After(time.Second * 1):
				debug("pump.update(): route timeout, dropping")
				defer delete(p.routes, r)
			}
		}
	}
}

func (p *LogsPump) RoutingFrom(id string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, monitoring := p.pumps[normalID(id)]
	return monitoring
}

func (p *LogsPump) Route(route *Route, logstream chan *Message) {
	p.mu.Lock()
	for _, pump := range p.pumps {
		if route.MatchContainer(
			normalID(pump.container.ID),
			normalName(pump.container.Name),
			pump.container.Config.Env) {

			pump.add(logstream, route)
			defer pump.remove(logstream)
		}
		//add MatchEnv
	}
	updates := make(chan *update)
	p.routes[updates] = struct{}{}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.routes, updates)
		p.mu.Unlock()
	}()
	for {
		select {
		case event := <-updates:
			switch event.Status {
			case "start", "restart":
				if route.MatchContainer(
					normalID(event.pump.container.ID),
					normalName(event.pump.container.Name),
					event.pump.container.Config.Env) {
					event.pump.add(logstream, route)
					defer event.pump.remove(logstream)
				}
			case "die":
				if strings.HasPrefix(route.FilterID, event.ID) {
					// If the route is just about a single container,
					// we can stop routing when it dies.
					return
				}
			}
		case <-route.Closer():
			return
		}
	}
}

type containerPump struct {
	sync.Mutex
	container  *docker.Container
	logstreams map[chan *Message]*Route
}

func newContainerPump(container *docker.Container, stdout, stderr io.Reader) *containerPump {
	cp := &containerPump{
		container:  container,
		logstreams: make(map[chan *Message]*Route),
	}
	filter := getopt("FILTER", "TOPIC")
	topic_env := getEnv(filter, container.Config.Env, "default")
	time_regexp, _ := time_regexp_map[topic_env]
	pump := func(source string, input io.Reader) {
		buf := bufio.NewReader(input)
		var line string
		line_count := 0
		maxable := false
		line, err := buf.ReadString('\n') //add exception judge,read one line
		if err != nil {
			if err != io.EOF {
				debug("pump.newContainerPump():", normalID(container.ID), source+":", err)
			}
			return
		}
		for {
			secline, err := buf.ReadString('\n') //add exception judge,read one line
			if err != nil {
				if err != io.EOF {
					debug("pump.newContainerPump():", normalID(container.ID), source+":", err)
					return
				}
				cp.send(&Message{
					Data:      strings.TrimSuffix(line, "\n"),
					Container: container,
					Time:      time.Now(),
					Source:    source,
				})
				return
			}
			if !time_regexp.MatchString(secline) {
				if maxable { //already maxable, drop all the left timeless, line is timeless now
					line = secline
					continue
				}
				if line_count < 10000 {
					line = line + secline
					line_count = line_count + 1
					continue
				}
				maxable = true //if timeless line exceed 10000, drop the left timeless lines
				line_count = 0
			} else {
				if maxable {
					//if maxable == true, then the line is timeless, drop it
					line = secline // line is good
					maxable = false
					continue
				}
			}
			cp.send(&Message{
				Data:      strings.TrimSuffix(line, "\n"),
				Container: container,
				Time:      time.Now(),
				Source:    source,
			})
			line = secline
		}
	}
	go pump("stdout", stdout)
	go pump("stderr", stderr)
	return cp
}

func (cp *containerPump) send(msg *Message) {
	cp.Lock()
	defer cp.Unlock()
	for logstream, route := range cp.logstreams {
		if !route.MatchMessage(msg) {
			continue
		}
		select {
		case logstream <- msg:
		case <-time.After(time.Second * 1):
			debug("pump.send(): send timeout, closing")
			// normal call to remove() triggered by
			// route.Closer() may not be able to grab
			// lock under heavy load, so we delete here
			defer delete(cp.logstreams, logstream)
		}
	}
}

func (cp *containerPump) add(logstream chan *Message, route *Route) {
	cp.Lock()
	defer cp.Unlock()
	cp.logstreams[logstream] = route
}

func (cp *containerPump) remove(logstream chan *Message) {
	cp.Lock()
	defer cp.Unlock()
	delete(cp.logstreams, logstream)
}
