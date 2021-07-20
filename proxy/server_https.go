package proxy

import (
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strconv"

	"github.com/AdguardTeam/golibs/log"
	"github.com/joomcode/errorx"
	"github.com/miekg/dns"
)

func (p *Proxy) createHTTPSListeners() error {
	for _, a := range p.HTTPSListenAddr {
		log.Info("Creating an HTTPS server")
		tcpListen, err := net.ListenTCP("tcp", a)
		if err != nil {
			return errorx.Decorate(err, "could not start HTTPS listener")
		}
		p.httpsListen = append(p.httpsListen, tcpListen)
		log.Info("Listening to https://%s", tcpListen.Addr())

		srv := &http.Server{
			TLSConfig:         p.TLSConfig.Clone(),
			Handler:           p,
			ReadHeaderTimeout: defaultTimeout,
			WriteTimeout:      defaultTimeout,
		}
		p.httpsServer = append(p.httpsServer, srv)
	}

	return nil
}

// serveHttps starts the HTTPS server
func (p *Proxy) listenHTTPS(srv *http.Server, l net.Listener) {
	log.Info("Listening to DNS-over-HTTPS on %s", l.Addr())
	err := srv.ServeTLS(l, "", "")

	if err != http.ErrServerClosed {
		log.Info("HTTPS server was closed unexpectedly: %s", err)
	} else {
		log.Info("HTTPS server was closed")
	}
}

// ServeHTTP is the http.RequestHandler implementation that handles DOH queries
// Here is what it returns:
// http.StatusBadRequest - if there is no DNS request data
// http.StatusUnsupportedMediaType - if request content type is not application/dns-message
// http.StatusMethodNotAllowed - if request method is not GET or POST
func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	log.Tracef("Incoming HTTPS request on %s", r.URL)

	var buf []byte
	var err error

	switch r.Method {
	case http.MethodGet:
		dnsParam := r.URL.Query().Get("dns")
		buf, err = base64.RawURLEncoding.DecodeString(dnsParam)
		if len(buf) == 0 || err != nil {
			log.Tracef("Cannot parse DNS request from %s", dnsParam)
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}
	case http.MethodPost:
		contentType := r.Header.Get("Content-Type")
		if contentType != "application/dns-message" {
			log.Tracef("Unsupported media type: %s", contentType)
			http.Error(w, http.StatusText(http.StatusUnsupportedMediaType), http.StatusUnsupportedMediaType)
			return
		}

		buf, err = ioutil.ReadAll(r.Body)
		if err != nil {
			log.Tracef("Cannot read the request body: %s", err)
			http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
			return
		}
		defer r.Body.Close()
	default:
		log.Tracef("Wrong HTTP method: %s", r.Method)
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}

	req := &dns.Msg{}
	if err = req.Unpack(buf); err != nil {
		log.Tracef("msg.Unpack: %s", err)
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}

	addr, _ := p.remoteAddr(r)

	d := p.newDNSContext(ProtoHTTPS, req)
	d.Addr = addr
	d.HTTPRequest = r
	d.HTTPResponseWriter = w

	err = p.handleDNSRequest(d)
	if err != nil {
		log.Tracef("error handling DNS (%s) request: %s", d.Proto, err)
	}
}

// Get a client IP address from HTTP headers that proxy servers may set
func getIPFromHTTPRequest(r *http.Request) net.IP {
	names := []string{
		"CF-Connecting-IP", "True-Client-IP", // set by CloudFlare servers
		"X-Real-IP",
	}
	for _, name := range names {
		s := r.Header.Get(name)
		ip := net.ParseIP(s)
		if ip != nil {
			return ip
		}
	}

	s := r.Header.Get("X-Forwarded-For")
	s = splitNext(&s, ',') // get left-most IP address
	ip := net.ParseIP(s)
	if ip != nil {
		return ip
	}

	return nil
}

// Writes a response to the DOH client
func (p *Proxy) respondHTTPS(d *DNSContext) error {
	resp := d.Res
	w := d.HTTPResponseWriter

	if resp == nil {
		// If no response has been written, indicate it via a 500 error
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return nil
	}

	bytes, err := resp.Pack()
	if err != nil {
		http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
		return errorx.Decorate(err, "couldn't convert message into wire format: %s", resp.String())
	}

	w.Header().Set("Server", "AdGuard DNS")
	w.Header().Set("Content-Type", "application/dns-message")
	_, err = w.Write(bytes)
	return err
}

func (p *Proxy) remoteAddr(r *http.Request) (net.Addr, error) {
	host, port, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return nil, err
	}

	portValue, err := strconv.Atoi(port)
	if err != nil {
		return nil, err
	}

	ip := getIPFromHTTPRequest(r)
	if ip != nil {
		log.Tracef("Using IP address from HTTP request: %s", ip)
	} else {
		ip = net.ParseIP(host)
		if ip == nil {
			return nil, fmt.Errorf("invalid IP: %s", host)
		}
	}

	return &net.TCPAddr{IP: ip, Port: portValue}, nil
}
