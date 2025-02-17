package main

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mosajjal/sniproxy/doh"
	doqclient "github.com/natesales/doqd/pkg/client"
	log "github.com/sirupsen/logrus"

	"github.com/miekg/dns"
)

// inDomainList returns true if the domain exists in the routeDomainList
// todo: this needs to be replaced by a few tst
func inDomainList(name string) bool {
	for _, item := range c.routeDomainList {
		if len(item) == 2 {
			if item[1] == "suffix" {
				if strings.HasSuffix(name, item[0]) {
					return true
				}
			} else if item[1] == "fqdn" {
				if name == item[0] {
					return true
				}
			} else if item[1] == "prefix" {
				if strings.HasPrefix(name, item[0]) {
					return true
				}
			}
		}
	}
	return false
}

var dnsClient struct {
	Doq        doqclient.Client
	Doh        doh.Client
	classicDNS dns.Client
}

func loadDomainsToList(Filename string) [][]string {
	log.Info("Loading the domain from file/url to a list")
	var lines [][]string
	var scanner *bufio.Scanner
	if strings.HasPrefix(Filename, "http://") || strings.HasPrefix(Filename, "https://") {
		log.Info("domain list is a URL, trying to fetch")
		http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
		client := http.Client{
			CheckRedirect: func(r *http.Request, via []*http.Request) error {
				r.URL.Opaque = r.URL.Path
				return nil
			},
		}
		resp, err := client.Get(Filename)
		if err != nil {
			log.Fatal(err)
		}
		log.Info("(re)fetching URL: ", Filename)
		defer resp.Body.Close()
		scanner = bufio.NewScanner(resp.Body)

	} else {
		file, err := os.Open(Filename)
		if err != nil {
			log.Fatal(err)
		}
		log.Info("(re)loading File: ", Filename)
		defer file.Close()
		scanner = bufio.NewScanner(file)
	}

	for scanner.Scan() {
		lowerCaseLine := strings.ToLower(scanner.Text())
		lines = append(lines, strings.Split(lowerCaseLine, ","))
	}
	log.Infof("%s loaded with %d lines", Filename, len(lines))
	return lines
}

func performExternalQuery(question dns.Question, server string) (*dns.Msg, time.Duration, error) {
	dnsURL, err := url.Parse(server)
	if err != nil {
		log.Fatalf("Invalid upstream DNS URL: %s", server)
	}
	msg := dns.Msg{
		MsgHdr: dns.MsgHdr{
			Id:               dns.Id(),
			RecursionDesired: true,
		},
		Question: []dns.Question{question},
	}

	if dnsURL.Scheme == "quic" {
		rmsg, err := dnsClient.Doq.SendQuery(msg)
		return &rmsg, 0, err

	}
	if dnsURL.Scheme == "https" {
		rmsg, t, err := dnsClient.Doh.SendQuery(msg)
		return &rmsg, t, err

	}
	return dnsClient.classicDNS.Exchange(&msg, dnsURL.Host)
}

func processQuestion(q dns.Question) ([]dns.RR, error) {
	if c.AllDomains || inDomainList(q.Name) {
		// Return the public IP.
		rr, err := dns.NewRR(fmt.Sprintf("%s A %s", q.Name, c.PublicIP))
		if err != nil {
			return nil, err
		}

		log.Printf("returned sniproxy address for domain: %s", q.Name)

		return []dns.RR{rr}, nil
	}

	// Otherwise do an upstream query and use that answer.
	resp, rtt, err := performExternalQuery(q, c.UpstreamDNS)
	if err != nil {
		return nil, err
	}

	log.Printf("returned origin address for domain: %s, rtt: %s", q.Name, rtt)

	return resp.Answer, nil
}
