package main

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mosajjal/sniproxy/doh"
	doqclient "github.com/natesales/doqd/pkg/client"
	doqserver "github.com/natesales/doqd/pkg/server"
	flag "github.com/spf13/pflag"

	"github.com/miekg/dns"
	log "github.com/sirupsen/logrus"
)

type runConfig struct {
	BindIP                    string     `json:"bindIP"`
	PublicIP                  string     `json:"publicIP"`
	UpstreamDNS               string     `json:"upstreamDNS"`
	DomainListPath            string     `json:"domainListPath"`
	DomainListRefreshInterval duration   `json:"domainListRefreshInterval"`
	BindDNSOverTCP            bool       `json:"bindDnsOverTcp"`
	BindDNSOverTLS            bool       `json:"bindDnsOverTls"`
	BindDNSOverQuic           bool       `json:"bindDnsOverQuic"`
	AllDomains                bool       `json:"allDomains"`
	routeDomainList           [][]string `json:"-"`
}

var c runConfig

func handle80(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "https://"+r.Host+r.RequestURI, http.StatusFound)
}

func pipe(conn1 net.Conn, conn2 net.Conn) {
	chan1 := getChannel(conn1)
	chan2 := getChannel(conn2)
	for {
		select {
		case b1 := <-chan1:
			if b1 == nil {
				return
			}
			conn2.Write(b1)
		case b2 := <-chan2:
			if b2 == nil {
				return
			}
			conn1.Write(b2)
		}
	}
}

func getChannel(conn net.Conn) chan []byte {
	c := make(chan []byte)
	go func() {
		b := make([]byte, 1024)
		for {
			n, err := conn.Read(b)
			if n > 0 {
				res := make([]byte, n)
				copy(res, b[:n])
				c <- res
			}
			if err != nil {
				c <- nil
				break
			}
		}
	}()
	return c
}

func getPublicIP() string {
	conn, _ := net.Dial("udp", "8.8.8.8:53")
	defer conn.Close()
	localAddr := conn.LocalAddr().String()
	idx := strings.LastIndex(localAddr, ":")
	ipaddr := localAddr[0:idx]
	if !net.ParseIP(ipaddr).IsPrivate() {
		return ipaddr
	}
	externalIP := ""
	// trying to get the public IP from multiple sources to see if they match.
	resp, err := http.Get("https://myexternalip.com/raw")
	if err == nil {
		defer resp.Body.Close()
		body, err := ioutil.ReadAll(resp.Body)
		if err == nil {
			externalIP = string(body)
		}

		// backup method of getting a public IP
		if externalIP == "" {
			// dig +short myip.opendns.com @208.67.222.222
			dnsRes, _, err := performExternalQuery(dns.Question{Name: "myip.opendns.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}, "208.67.222.222")
			if err != nil {
				return err.Error()
			}
			externalIP = dnsRes.Answer[0].(*dns.A).A.String()
		}

		if externalIP != "" {
			return externalIP
		}
		log.Fatalf("Could not automatically find the public IP address. Please specify it in the configuration.")

	}
	return ""

}

func lookupDomain4(domain string) (net.IP, error) {
	if !strings.HasSuffix(domain, ".") {
		domain = domain + "."
	}
	rAddrDNS, _, err := performExternalQuery(dns.Question{Name: domain, Qtype: dns.TypeA, Qclass: dns.ClassINET}, c.UpstreamDNS)
	if err != nil {
		log.Println(err)
		return nil, err
	}
	if len(rAddrDNS.Answer) > 0 {
		if rAddrDNS.Answer[0].Header().Rrtype == dns.TypeCNAME {
			return lookupDomain4(rAddrDNS.Answer[0].(*dns.CNAME).Target)
		}
		if rAddrDNS.Answer[0].Header().Rrtype == dns.TypeA {
			return rAddrDNS.Answer[0].(*dns.A).A, nil
		}
	}
	return nil, fmt.Errorf("Unknown type")
}

func handle443(conn net.Conn) error {
	incoming := make([]byte, 2048)
	n, err := conn.Read(incoming)
	if err != nil {
		log.Println(err)
		return err
	}
	sni, err := GetHostname(incoming)
	if err != nil {
		log.Println(err)
		return err
	}
	// rAddrDns, err := performExternalQuery(dns.Question{Name: sni + ".", Qtype: dns.TypeA, Qclass: dns.ClassINET}, *upstreamDNS)
	// if err != nil {
	// 	log.Println(err)
	// 	return err
	// }
	// rAddr := rAddrDns.Answer[0].(*dns.A).A
	rAddr, err := lookupDomain4(sni)
	if err != nil || rAddr == nil {
		log.Println(err)
		return err
	}
	target, err := net.DialTCP("tcp", nil, &net.TCPAddr{IP: rAddr, Port: 443})
	if err != nil {
		log.Println("could not connect to target", err)
		conn.Close()
		return err
	}
	defer target.Close()
	target.Write(incoming[:n])
	pipe(conn, target)
	return nil
}

func handleDNS(w dns.ResponseWriter, r *dns.Msg) {
	m := new(dns.Msg)
	m.SetReply(r)
	m.Compress = false

	if r.Opcode != dns.OpcodeQuery {
		m.SetRcode(r, dns.RcodeNotImplemented)
		w.WriteMsg(m)
		return
	}

	for _, q := range m.Question {
		answers, err := processQuestion(q)
		if err != nil {
			log.Error(err)
			continue
		}
		m.Answer = append(m.Answer, answers...)
	}

	w.WriteMsg(m)
}

func handleError(err error) {
	if err != nil {
		log.Fatalln(err)
	}
}

func runHTTP() {
	http.HandleFunc("/", handle80)
	server := http.Server{
		Addr: ":80",
	}
	server.ListenAndServe()
}

func runHTTPS() {
	l, err := net.Listen("tcp", c.BindIP+":443")

	handleError(err)
	defer l.Close()
	for {
		c, err := l.Accept()
		handleError(err)
		go func() {
			go handle443(c)
			<-time.After(30 * time.Second)
			c.Close()
		}()
	}
}

func runDNS() {
	dns.HandleFunc(".", handleDNS)
	// start DNS UDP serverUdp
	go func() {
		serverUDP := &dns.Server{Addr: ":53", Net: "udp"}
		log.Printf("Started UDP DNS on %s:%d -- listening", "0.0.0.0", 53)
		err := serverUDP.ListenAndServe()
		defer serverUDP.Shutdown()
		if err != nil {
			log.Fatalf("Failed to start server: %s\nYou can run the following command to pinpoint which process is listening on port 53\nsudo ss -pltun -at '( dport = :53 or sport = :53 )'", err.Error())
		}
	}()

	// start DNS UDP serverTcp
	if c.BindDNSOverTCP {
		go func() {
			serverTCP := &dns.Server{Addr: ":53", Net: "tcp"}
			log.Printf("Started TCP DNS on %s:%d -- listening", "0.0.0.0", 53)
			err := serverTCP.ListenAndServe()
			defer serverTCP.Shutdown()
			if err != nil {
				log.Fatalf("Failed to start server: %s\nYou can run the following command to pinpoint which process is listening on port 53\nsudo ss -pltun -at '( dport = :53 or sport = :53 )'", err.Error())
			}
		}()
	}

	// start DNS UDP serverTls
	if c.BindDNSOverTLS {
		go func() {
			_, _, err := GenerateSelfSignedCertKey(c.PublicIP, nil, nil, os.TempDir())
			if err != nil {
				log.Fatal("fatal Error: ", err)
			}
			crt, err := tls.LoadX509KeyPair(filepath.Join(os.TempDir(), c.PublicIP+".crt"), filepath.Join(os.TempDir(), c.PublicIP+".key"))
			if err != nil {
				log.Fatalln(err.Error())
			}
			tlsConfig := &tls.Config{}
			tlsConfig.Certificates = []tls.Certificate{crt}

			serverTLS := &dns.Server{Addr: ":853", Net: "tcp-tls", TLSConfig: tlsConfig}
			log.Printf("Started DoT on %s:%d -- listening", "0.0.0.0", 853)
			err = serverTLS.ListenAndServe()
			defer serverTLS.Shutdown()
			if err != nil {
				log.Fatalf("Failed to start server: %s\n ", err.Error())
			}
		}()
	}

	if c.BindDNSOverQuic {

		_, _, err := GenerateSelfSignedCertKey(c.PublicIP, nil, nil, os.TempDir())
		if err != nil {
			log.Fatal("fatal Error: ", err)
		}
		crt, err := tls.LoadX509KeyPair(filepath.Join(os.TempDir(), c.PublicIP+".crt"), filepath.Join(os.TempDir(), c.PublicIP+".key"))
		if err != nil {
			log.Fatalln(err.Error())
		}
		tlsConfig := &tls.Config{}
		tlsConfig.Certificates = []tls.Certificate{crt}

		// Create the QUIC listener
		doqServer, err := doqserver.New(":8853", crt, "127.0.0.1:53", true)
		if err != nil {
			log.Fatalln(err.Error())
		}

		// Accept QUIC connections
		log.Infof("Starting QUIC listener on %s\n", ":443")
		go doqServer.Listen()

	}
}

func main() {
	flag.StringVar(&c.BindIP, "bindIP", "0.0.0.0", "Bind 443 and 80 to a Specific IP Address. Doesn't apply to DNS Server. DNS Server always listens on 0.0.0.0")
	flag.StringVar(&c.UpstreamDNS, "upstreamDNS", "udp://1.1.1.1:53", "Upstream DNS URI. examples: udp://1.1.1.1:53, tcp://1.1.1.1:53, tcp-tls://1.1.1.1:853, https://dns.google/dns-query")
	flag.StringVar(&c.DomainListPath, "domainListPath", "", "Path to the domain list. eg: /tmp/domainlist.log")
	flag.StringVar(&c.PublicIP, "publicIP", getPublicIP(), "Public IP of the server, reply address of DNS queries")
	flag.DurationVar(&c.DomainListRefreshInterval.Duration, "domainListRefreshInterval", 60*time.Minute, "Interval to re-fetch the domain list, default: 1 hour")
	flag.BoolVar(&c.AllDomains, "allDomains", false, "Route all HTTP(s) traffic through the SNI proxy")
	flag.BoolVar(&c.BindDNSOverTCP, "bindDnsOverTcp", false, "enable DNS over TCP as well as UDP")
	flag.BoolVar(&c.BindDNSOverTLS, "bindDnsOverTls", false, "enable DNS over TLS as well as UDP")
	flag.BoolVar(&c.BindDNSOverQuic, "bindDnsOverQuic", false, "enable DNS over QUIC as well as UDP")

	config := flag.StringP("config", "c", "", "path to JSON configuration file")

	flag.Parse()
	if *config != "" {
		configFile, err := os.Open(*config)
		if err != nil {
			log.Fatalf("failed to open config file: %s", err.Error())
		}
		defer configFile.Close()
		fileStat, err := configFile.Stat()
		configBytes := make([]byte, fileStat.Size())
		_, err = configFile.Read(configBytes)

		err = json.Unmarshal(configBytes, &c)
		if err != nil {
			log.Fatalf("failed to parse config file: %s", err.Error())
		}
	}

	if c.DomainListPath == "" {
		log.Warnf("Domain list (--domainListPath) is not specified, routing ALL domains through the SNI proxy")
		c.AllDomains = true
	}
	if c.PublicIP != "" {
		log.Infof("Using Public IP: %s", c.PublicIP)
	} else {
		log.Fatalf("Could not automatically determine public IP. you should provide it manually using --publicIP")
	}

	// set up upstream DNS clients
	dnsURL, err := url.Parse(c.UpstreamDNS)
	if err != nil {
		log.Fatalf("Invalid upstream DNS URL: %s", c.UpstreamDNS)
	}
	if dnsURL.Scheme == "quic" {
		c, err := doqclient.New(dnsURL.Host, true, true)
		if err != nil {
			log.Fatalf("Failed to connect to upstream DNS: %s", err.Error())
		}
		dnsClient.Doq = c
	} else if dnsURL.Scheme == "https" {
		c, err := doh.New(*dnsURL, true, true)
		if err != nil {
			log.Fatalf("Failed to connect to upstream DNS: %s", err.Error())
		}
		dnsClient.Doh = c
	} else {
		c := dns.Client{
			Net: dnsURL.Scheme,
		}
		// this dial is not used and it's only good for testing
		_, err := c.Dial(dnsURL.Host)
		if err != nil {
			log.Fatalf("Failed to connect to upstream DNS: %s", err.Error())
		}
		dnsClient.classicDNS = c
	}

	go runHTTP()
	go runHTTPS()
	go runDNS()

	// fetch domain list and refresh them periodically
	if !c.AllDomains {
		c.routeDomainList = loadDomainsToList(c.DomainListPath)
		for range time.NewTicker(c.DomainListRefreshInterval.Duration).C {
			c.routeDomainList = loadDomainsToList(c.DomainListPath)
		}
	} else {
		select {}
	}
}
