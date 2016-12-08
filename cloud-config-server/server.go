// cloud-config-server starts an HTTP server, which can be accessed
// via URLs in the form of
//
//   http://<addr:port>?mac=aa:bb:cc:dd:ee:ff
//
// and returns the cloud-config YAML file specificially tailored for
// the node whose primary NIC's MAC address matches that specified in
// above URL.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"path"
	"strings"

	"github.com/golang/glog"

	"github.com/gorilla/mux"
	"github.com/k8sp/sextant/cloud-config-server/cache"
	"github.com/k8sp/sextant/cloud-config-server/certgen"
	"github.com/k8sp/sextant/cloud-config-server/clusterdesc"
	cctemplate "github.com/k8sp/sextant/cloud-config-server/template"
	"github.com/topicai/candy"
	"gopkg.in/yaml.v2"
)

func main() {
	clusterDesc := flag.String("cluster-desc", "./cluster-desc.yml", "Configurations for a k8s cluster.")
	ccTemplateDir := flag.String("cloud-config-dir", "./cloud-config.template", "cloud-config file template.")
	caCrt := flag.String("ca-crt", "", "CA certificate file, in PEM format")
	caKey := flag.String("ca-key", "", "CA private key file, in PEM format")
	addr := flag.String("addr", ":8080", "Listening address")
	staticDir := flag.String("dir", "./static/", "The directory to serve files from. Default is ./static/")
	validate := flag.Bool("validate", false, "Validate cluster-desc.yaml and the generated cloud-config file.")
	flag.Parse()

	if len(*caCrt) == 0 || len(*caKey) == 0 {
		glog.Info("No ca.pem or ca-key.pem provided, generating now...")
		*caKey, *caCrt = certgen.GenerateRootCA("./")
	}
	// valid caKey and caCrt file is ready
	candy.Must(fileExist(*caCrt))
	candy.Must(fileExist(*caKey))

	// Validate cluster-desc.yaml, the generated cloud-config.yaml which generated by the mac in cluster-desc
	if *validate == true {
		glog.Info("Checking %s ...", *clusterDesc)
		err := validation(*clusterDesc, *ccTemplateDir, *caKey, *caCrt, *staticDir)
		if err != nil {
			glog.Info("Failed: \n" + err.Error())
			os.Exit(1)
		}
		glog.Info("Successed!")
		os.Exit(0)
	}
	glog.Info("Cloud-config server start Listenning...")
	l, e := net.Listen("tcp", *addr)
	candy.Must(e)

	// start and run the HTTP server
	router := mux.NewRouter().StrictSlash(true)
	router.HandleFunc("/cloud-config/{mac}", makeCloudConfigHandler(*clusterDesc, *ccTemplateDir, *caKey, *caCrt))
	router.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir(*staticDir))))

	glog.Fatal(http.Serve(l, router))
}

// Validate cluster-desc.yaml and check the generated cloud-config file format.
func validation(clusterDescFile string, ccTemplateDir string, caKey, caCrt, dir string) error {
	clusterDesc, err := ioutil.ReadFile(clusterDescFile)
	candy.Must(err)
	_, direrr := os.Stat(ccTemplateDir)
	if os.IsNotExist(direrr) {
		return direrr
	}

	c := &clusterdesc.Cluster{}
	// validate cluster-desc format
	err = yaml.Unmarshal(clusterDesc, c)
	if err != nil {
		return errors.New("cluster-desc file formate failed: " + err.Error())
	}

	// flannel backend only support host-gw and udp for now
	if c.FlannelBackend != "host-gw" && c.FlannelBackend != "udp" && c.FlannelBackend != "vxlan" {
		return errors.New("Flannl backend should be host-gw or udp.")
	}

	// Inlucde one master and one etcd member at least
	countEtcdMember := 0
	countKubeMaster := 0
	for _, node := range c.Nodes {
		if node.EtcdMember {
			countEtcdMember++
		}
		if node.KubeMaster {
			countKubeMaster++
		}
	}
	if countEtcdMember == 0 || countKubeMaster == 0 {
		return errors.New("Cluster description yaml should include one master and one etcd member at least.")
	}

	if len(c.SSHAuthorizedKeys) == 0 {
		return errors.New("Cluster description yaml should include one ssh key.")
	}

	var ccTmplBuffer bytes.Buffer
	var macList []string
	macList = append(macList, "00:00:00:00:00:00")
	for _, n := range c.Nodes {
		macList = append(macList, n.Mac())
	}
	for _, mac := range macList {
		//err = cctemplate.Execute(tmpl, c, mac, caKey, caCrt, &ccTmplBuffer)
		err = cctemplate.Execute(&ccTmplBuffer, mac, "cc-template", ccTemplateDir, clusterDescFile, caKey, caCrt)
		if err != nil {
			return errors.New("Generate cloud-config failed with mac: " + mac + "\n" + err.Error())
		}

		yml := make(map[interface{}]interface{})
		err = yaml.Unmarshal(ccTmplBuffer.Bytes(), yml)
		if err != nil {
			return errors.New("Generate cloud-config format failed with mac: " + mac + "\n" + err.Error())
		}
		ccTmplBuffer.Reset()
	}
	return nil
}

// makeCloudConfigHandler generate a HTTP server handler to serve cloud-config
// fetching requests
func makeCloudConfigHandler(clusterDescFile string, ccTemplateDir string, caKey, caCrt string) http.HandlerFunc {
	return makeSafeHandler(func(w http.ResponseWriter, r *http.Request) {
		mac := strings.ToLower(mux.Vars(r)["mac"])
		candy.Must(cctemplate.Execute(w, mac, "cc-template", ccTemplateDir, clusterDescFile, caKey, caCrt))
	})
}

func makeSafeHandler(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				http.Error(w, fmt.Sprint(err), http.StatusInternalServerError)
			}
		}()
		h(w, r)
	}
}

func makeCacheGetter(url, fn string) func() []byte {
	if len(fn) == 0 {
		dir, e := ioutil.TempDir("", "")
		candy.Must(e)
		fn = path.Join(dir, "localfile")
	}
	c := cache.New(url, fn)
	return func() []byte { return c.Get() }
}

func fileExist(fn string) error {
	_, err := os.Stat(fn)
	if err != nil || os.IsNotExist(err) {
		return errors.New("file " + fn + " is not ready.")
	}
	return nil
}
