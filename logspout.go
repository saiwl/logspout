package main

import (
	"fmt"
	"log"
	"os"
	"time"
	"strings"
	"io/ioutil"
	"strconv"
	"text/tabwriter"
	"encoding/binary"

	"github.com/gliderlabs/logspout/router"
)

var Version string

func BytesToInt64(buf []byte) int64 {
	return int64(binary.BigEndian.Uint64(buf))
}

func Int64ToBytes(i int64) []byte {
	var buf = make([]byte, 8)
	binary.BigEndian.PutUint64(buf, uint64(i))
	return buf
}

func getopt(name, dfault string) string {
	value := os.Getenv(name)
	if value == "" {
		value = dfault
	}
	return value
}

func writeSinceTime(path string, sinceTime time.Time) error {
	//log.Println("write sincetime: ", sinceTime.Unix())
	return ioutil.WriteFile(path, Int64ToBytes(sinceTime.Unix()), 0644)
}

//get sincetime from file, if file not exists, return time.Now()
func getSinceTime(path string) time.Time {
	if _, err := os.Stat(path); err == nil {
		sinceTime, err := ioutil.ReadFile(path)
		if err != nil {
			log.Println("read sinceTime error:", err)
		}
		return time.Unix(BytesToInt64(sinceTime), 0)
	}
	return time.Now()
}

func persisten_sinceTime() {
	for{
		writeSinceTime("sincetime", time.Now())
		time.Sleep(time.Second * 1)
	}
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println(Version)
		os.Exit(0)
	}
	sinceTime := getSinceTime("sincetime")
	os.Setenv("SINCE_TIME", strconv.FormatInt(sinceTime.Unix(), 10))
	fmt.Println("#sincetime:", sinceTime.String())

	fmt.Printf("# logspout %s by gliderlabs\n", Version)
	fmt.Printf("# adapters: %s\n", strings.Join(router.AdapterFactories.Names(), " "))
	fmt.Printf("# options : ")
	if getopt("DEBUG", "") != "" {
		fmt.Printf("debug:%s ", getopt("DEBUG", ""))
	}
	fmt.Printf("persist:%s\n", getopt("ROUTESPATH", "/mnt/routes"))

	var jobs []string
	for _, job := range router.Jobs.All() {
		err := job.Setup()
		if err != nil {
			fmt.Println("!!", err)
			os.Exit(1)
		}
		if job.Name() != "" {
			jobs = append(jobs, job.Name())
		}
	}
	fmt.Printf("# jobs    : %s\n", strings.Join(jobs, " "))

	go persisten_sinceTime()

	routes, _ := router.Routes.GetAll()
	if len(routes) > 0 {
		fmt.Println("# routes  :")
		w := new(tabwriter.Writer)
		w.Init(os.Stdout, 0, 8, 0, '\t', 0)
		fmt.Fprintln(w, "#   ADAPTER\tADDRESS\tCONTAINERS\tSOURCES\tOPTIONS")
		for _, route := range routes {
			fmt.Fprintf(w, "#   %s\t%s\t%s\t%s\t%s\n",
				route.Adapter,
				route.Address,
				route.FilterID+route.FilterName,
				strings.Join(route.FilterSources, ","),
				route.Options)
		}
		w.Flush()
	} else {
		fmt.Println("# routes  : none")
	}

	for _, job := range router.Jobs.All() {
		job := job
		go func() {
			log.Fatalf("%s ended: %s", job.Name(), job.Run())
		}()
	}

	select {}
}
