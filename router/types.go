//go:generate go-extpoints . AdapterFactory HttpHandler AdapterTransport LogRouter Job
package router

import (
	"net"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/fsouza/go-dockerclient"
)

// Extension type for adding HTTP endpoints
type HttpHandler func() http.Handler

// Extension type for adding new log adapters
type AdapterFactory func(route *Route) (LogAdapter, error)

// Extension type for connection transports used by adapters
type AdapterTransport interface {
	Dial(addr string, options map[string]string) (net.Conn, error)
}

// LogAdapters are streamed logs
type LogAdapter interface {
	Stream(logstream chan *Message)
}

type Job interface {
	Run() error
	Setup() error
	Name() string
}

// LogRouters send logs to LogAdapters via Routes
type LogRouter interface {
	RoutingFrom(containerID string) bool
	Route(route *Route, logstream chan *Message)
}

// RouteStores are collections of Routes
type RouteStore interface {
	Get(id string) (*Route, error)
	GetAll() ([]*Route, error)
	Add(route *Route) error
	Remove(id string) bool
}

// Messages are log messages
type Message struct {
	Container *docker.Container
	Source    string
	Data      string
	Time      time.Time
}

// Routes represent what subset of logs should go where
type Route struct {
	ID            string            `json:"id"`
	FilterID      string            `json:"filter_id,omitempty"`
	FilterName    string            `json:"filter_name,omitempty"`
	FilterSources []string          `json:"filter_sources,omitempty"`
	/*env value in container, use to filter, the env key can be set by env*/
	FilterEnv     string            `json:"filter_env,omitempty"`
	Adapter       string            `json:"adapter"`
	Address       string            `json:"address"`
	Options       map[string]string `json:"options,omitempty"`
	adapter       LogAdapter
	closer        chan bool
	closerRcv     <-chan bool // used instead of closer when set
}

func (r *Route) AdapterType() string {
	return strings.Split(r.Adapter, "+")[0]
}

func (r *Route) AdapterTransport(dfault string) string {
	parts := strings.Split(r.Adapter, "+")
	if len(parts) > 1 {
		return parts[1]
	}
	return dfault
}

func (r *Route) Closer() <-chan bool {
	if r.closerRcv != nil {
		return r.closerRcv
	}
	return r.closer
}

func (r *Route) OverrideCloser(closer <-chan bool) {
	r.closerRcv = closer
}

func (r *Route) Close() {
	r.closer <- true
}

func (r *Route) matchAll() bool {
	if r.FilterID == "" && r.FilterName == "" && len(r.FilterSources) == 0 {
		return true
	}
	return false
}

func (r *Route) MultiContainer() bool {
	return r.matchAll() || strings.Contains(r.FilterName, "*")
}
//based on container's ID and name to filter
func (r *Route) MatchContainer(id, name string, envs []string) bool {
	filter := getopt("FILTER", "TOPIC")
	topic := getEnv(filter, envs, "default")
	if topic != r.FilterEnv {
		return false
	}
	if r.matchAll() {
		return true
	}
	if r.FilterID != "" && !strings.HasPrefix(id, r.FilterID) {
		return false
	}
	match, err := path.Match(r.FilterName, name)
	if err != nil || (r.FilterName != "" && !match) {
		return false
	}
	return true
}

func (r *Route) MatchMessage(message *Message) bool {
	if r.matchAll() {
		return true
	}
	if len(r.FilterSources) > 0 && !contains(r.FilterSources, message.Source) {
		return false
	}
	return true
}

func getEnv(env_key string, envs []string, dfault string) string {
	for _, env := range envs {
		if strings.HasPrefix(env, env_key + "=") {
			topic := strings.Split(env, "=")[1]
			return topic
		}
	}
	return dfault
}

func contains(strs []string, str string) bool {
	for _, s := range strs {
		if s == str {
			return true
		}
	}
	return false
}
