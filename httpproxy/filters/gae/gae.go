package gae

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cloudflare/golibs/lrucache"
	"github.com/golang/glog"
	"golang.org/x/net/http2"

	"../../../helpers"
	"../../../storage"
	"../../filters"
	"../../transport/direct"
	"../../transport/gae"
)

const (
	filterName string = "gae"
)

const (
	DefaultGAEScheme string = "https"
	DefaultGAEDomain string = "appspot.com"
	DefaultGAEPath   string = "/_gh/"
)

type Config struct {
	AppIDs      []string
	Scheme      string
	Domain      string
	Path        string
	Password    string
	SSLVerify   bool
	IPv6Only    bool
	Sites       []string
	Site2Alias  map[string]string
	HostMap     map[string][]string
	ForceHTTPS  []string
	ForceGAE    []string
	DNSServers  []string
	IPBlackList []string
	Transport   struct {
		Dialer struct {
			Timeout        int
			KeepAlive      int
			DualStack      bool
			RetryTimes     int
			RetryDelay     float32
			DNSCacheExpiry int
			DNSCacheSize   uint
			Level          int
		}
		DisableKeepAlives     bool
		DisableCompression    bool
		ResponseHeaderTimeout int
		MaxIdleConnsPerHost   int
	}
}

type Filter struct {
	Config
	GAETransport      *gae.Transport
	DirectTransport   *http.Transport
	ForceHTTPSMatcher *helpers.HostMatcher
	ForceGAEMatcher   *helpers.HostMatcher
	SiteMatcher       *helpers.HostMatcher
	DirectSiteMatcher *helpers.HostMatcher
}

func init() {
	filename := filterName + ".json"
	config := new(Config)
	err := storage.ReadJsonConfig(storage.LookupConfigStoreURI(filterName), filename, config)
	if err != nil {
		glog.Fatalf("storage.ReadJsonConfig(%#v) failed: %s", filename, err)
	}

	if config.Scheme == "" {
		config.Scheme = DefaultGAEScheme
	}

	if config.Domain == "" {
		config.Domain = DefaultGAEDomain
	}

	if config.Path == "" {
		config.Path = DefaultGAEPath
	}

	err = filters.Register(filterName, &filters.RegisteredFilter{
		New: func() (filters.Filter, error) {
			return NewFilter(config)
		},
	})

	if err != nil {
		glog.Fatalf("Register(%#v) error: %s", filterName, err)
	}
}

func NewFilter(config *Config) (filters.Filter, error) {
	dnsServers := make([]net.IP, 0)
	for _, s := range config.DNSServers {
		if ip := net.ParseIP(s); ip != nil {
			dnsServers = append(dnsServers, ip)
		}
	}

	d := &direct.MultiDialer{
		Dialer: net.Dialer{
			KeepAlive: time.Duration(config.Transport.Dialer.KeepAlive) * time.Second,
			Timeout:   time.Duration(config.Transport.Dialer.Timeout) * time.Second,
			DualStack: config.Transport.Dialer.DualStack,
		},
		IPv6Only:        config.IPv6Only,
		TLSConfig:       nil,
		Site2Alias:      helpers.NewHostMatcherWithString(config.Site2Alias),
		IPBlackList:     helpers.NewHostMatcher(config.IPBlackList),
		HostMap:         config.HostMap,
		DNSServers:      dnsServers,
		DNSCache:        lrucache.NewLRUCache(config.Transport.Dialer.DNSCacheSize),
		DNSCacheExpiry:  time.Duration(config.Transport.Dialer.DNSCacheExpiry) * time.Second,
		TCPConnDuration: lrucache.NewLRUCache(8192),
		TCPConnError:    lrucache.NewLRUCache(8192),
		TLSConnDuration: lrucache.NewLRUCache(8192),
		TLSConnError:    lrucache.NewLRUCache(8192),
		ConnExpiry:      5 * time.Minute,
		Level:           config.Transport.Dialer.Level,
	}

	tr := &http.Transport{
		Dial:                  d.Dial,
		DialTLS:               d.DialTLS,
		DisableKeepAlives:     config.Transport.DisableKeepAlives,
		DisableCompression:    config.Transport.DisableCompression,
		ResponseHeaderTimeout: time.Duration(config.Transport.ResponseHeaderTimeout) * time.Second,
		MaxIdleConnsPerHost:   config.Transport.MaxIdleConnsPerHost,
	}

	err := http2.ConfigureTransport(tr)
	if err != nil {
		glog.Infof("GAE: Error enabling Transport HTTP/2 support: %v", err)
	}

	servers := make([]gae.Server, 0)
	for _, appid := range config.AppIDs {
		var rawurl string
		switch strings.Count(appid, ".") {
		case 0, 1:
			rawurl = fmt.Sprintf("%s://%s.%s%s", config.Scheme, appid, config.Domain, config.Path)
		default:
			rawurl = fmt.Sprintf("%s://%s.%s%s", config.Scheme, appid, config.Path)
		}
		u, err := url.Parse(rawurl)
		if err != nil {
			return nil, err
		}

		server := gae.Server{
			URL:       u,
			Password:  config.Password,
			SSLVerify: config.SSLVerify,
		}

		servers = append(servers, server)
	}

	return &Filter{
		Config: *config,
		GAETransport: &gae.Transport{
			RoundTripper: tr,
			MultiDialer:  d,
			Servers:      servers,
		},
		DirectTransport:   tr,
		ForceHTTPSMatcher: helpers.NewHostMatcher(config.ForceHTTPS),
		ForceGAEMatcher:   helpers.NewHostMatcher(config.ForceGAE),
		SiteMatcher:       helpers.NewHostMatcher(config.Sites),
		DirectSiteMatcher: helpers.NewHostMatcherWithString(config.Site2Alias),
	}, nil
}

func (p *Filter) FilterName() string {
	return filterName
}

func (f *Filter) RoundTrip(ctx *filters.Context, req *http.Request) (*filters.Context, *http.Response, error) {
	if !f.SiteMatcher.Match(req.Host) {
		return ctx, nil, nil
	}

	var tr http.RoundTripper = f.GAETransport
	prefix := "FETCH"

	if req.URL.Scheme == "http" && f.ForceHTTPSMatcher.Match(req.Host) {
		if !strings.HasPrefix(req.Header.Get("Referer"), "https://") {
			u := strings.Replace(req.URL.String(), "http://", "https://", 1)
			glog.V(2).Infof("GAE FORCEHTTPS get raw url=%v, redirect to %v", req.URL.String(), u)
			return ctx, &http.Response{
				Status:     "301 Moved Permanently",
				StatusCode: http.StatusMovedPermanently,
				Proto:      "HTTP/1.1",
				ProtoMajor: 1,
				ProtoMinor: 1,
				Header: http.Header{
					"Location": []string{u},
				},
				Request:       req,
				Close:         true,
				ContentLength: -1,
			}, nil
		}
	}

	if f.DirectSiteMatcher.Match(req.Host) {
		if req.URL.Path == "/url" {
			if u := req.URL.Query().Get("url"); u != "" {
				glog.V(2).Infof("GAE REDIRECT get raw url=%v, redirect to %v", req.URL.String(), u)
				return ctx, &http.Response{
					Status:     "302 Found",
					StatusCode: http.StatusFound,
					Proto:      "HTTP/1.1",
					ProtoMajor: 1,
					ProtoMinor: 1,
					Header: http.Header{
						"Location": []string{u},
					},
					Request:       req,
					Close:         true,
					ContentLength: -1,
				}, nil
			}
		}

		if req.URL.Scheme != "http" && !f.ForceGAEMatcher.Match(req.Host) {
			tr = f.DirectTransport
			prefix = "DIRECT"
		}
	}

	resp, err := tr.RoundTrip(req)

	if err != nil {
		if nerr, ok := err.(net.Error); ok && nerr.Timeout() {
			f.DirectTransport.CloseIdleConnections()
		}
		return ctx, nil, err
	} else {
		glog.Infof("%s \"GAE %s %s %s %s\" %d %s", req.RemoteAddr, prefix, req.Method, req.URL.String(), req.Proto, resp.StatusCode, resp.Header.Get("Content-Length"))
	}

	return ctx, resp, nil
}
