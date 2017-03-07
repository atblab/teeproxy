package main

import (
	"bytes"
	"crypto/tls"
	"flag"
	"fmt"
	log "github.com/Sirupsen/logrus"
	"github.com/satori/go.uuid"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"runtime"
	"time"
)

// Console flags
var (
	listen                = flag.String("l", ":8888", "port to accept requests")
	targetProduction      = flag.String("a", "http://localhost:8080", "where production (A Side) traffic goes. http://localhost:8080/production")
	altTarget             = flag.String("b", "http://localhost:8081", "where alternate  (B Side) traffic goes. response are skipped. http://localhost:8081/test")
	debug                 = flag.Int("debug", 0, "more logging, showing ignored output")
	productionTimeout     = flag.Int("a.timeout", 3, "timeout in seconds for production traffic")
	alternateTimeout      = flag.Int("b.timeout", 3, "timeout in seconds for alternate site traffic")
	jsonLogging           = flag.Bool("j", false, "write the logs in json for easier processing")
	productionHostRewrite = flag.Bool("a.rewrite", false, "rewrite the host header when proxying production traffic")
	alternateHostRewrite  = flag.Bool("b.rewrite", false, "rewrite the host header when proxying alternate site traffic")
	percent               = flag.Float64("p", 100.0, "float64 percentage of traffic to send to alternate (B Side)")
	tlsPrivateKey         = flag.String("key.file", "", "path to the TLS private key file")
	tlsCertificate        = flag.String("cert.file", "", "path to the TLS certificate file")
	version               = flag.Bool("v", false, "show version number")
)

// handler contains the address of the main Target and the one for the Alternative target
type handler struct {
	Target      string
	Alternative string
	Randomizer  rand.Rand
}

// ServeHTTP duplicates the incoming request (req)
// and does the request to the Target
// and the Alternate target discading the Alternate response
func (h handler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	var productionRequest, alternativeRequest *http.Request
	uid := uuid.NewV4()
	if *percent == 100.0 || h.Randomizer.Float64()*100 < *percent {
		alternativeRequest, productionRequest = DuplicateRequest(req)

		//
		// B-Side (Alternate) Processing
		//
		go func() {

			defer func() {
				if r := recover(); r != nil {
					log.Warn(fmt.Sprintf("Recovered in f %s", r))
				}
			}()

			//
			// B-Side (Alternate) - Create Connection
			//
			alt_url, err := url.Parse(h.Alternative)
			var clientTcpConn net.Conn

			if alt_url.Scheme == "https" {
				clientTcpConn, err = tls.Dial("tcp", alt_url.Host, &tls.Config{InsecureSkipVerify: true})
			} else {
				clientTcpConn, err = net.DialTimeout("tcp", alt_url.Host, time.Duration(time.Duration(*alternateTimeout)*time.Second))
			}
			if err != nil {
				log.Error(fmt.Sprintf("(B-Side) Failed to connect to host %s for url prefix", alt_url.Host, h.Alternative))
				return
			}

			//
			// B-Side (Alternate) - Handle Request
			//
			clientHttpConn := httputil.NewClientConn(clientTcpConn, nil) // Start a new HTTP connection on it
			defer clientHttpConn.Close()                                 // Close the connection to the server
			if *alternateHostRewrite || alt_url.Scheme == "https" {
				alternativeRequest.Host = alt_url.Host
			}
			log.WithFields(log.Fields{
				"uuid":                  uid,
				"side":                  "B-Side",
				"request_method":        alternativeRequest.Method,
				"request_path":          alternativeRequest.URL.Path,
				"request_proto":         alternativeRequest.Proto,
				"request_host":          alternativeRequest.Host,
				"request_contentlength": alternativeRequest.ContentLength,
			}).Info("Proxy Request")
			err = clientHttpConn.Write(alternativeRequest) // Pass on the request
			if err != nil {
				log.Error(fmt.Sprintf("(B-Side) Failed to send to %s: %v", h.Alternative, err))
				return
			}
			b_resp, err := clientHttpConn.Read(alternativeRequest) // Read back the reply
			if err != nil && err != httputil.ErrPersistEOF {
				log.Error(fmt.Sprintf("(B-Side) Failed to receive from %s: %v", h.Alternative, err))
				return
			}
			log.WithFields(log.Fields{
				"uuid":                   uid,
				"side":                   "B-Side",
				"response_code":          b_resp.StatusCode,
				"response_contentlength": b_resp.ContentLength,
			}).Info("Proxy Response")

		}()
	} else {
		productionRequest = req
	}

	//
	// A-Side (Target) Processing
	//
	defer func() {
		if r := recover(); r != nil {
			log.Warn(fmt.Sprintf("Recovered in f %s", r))
		}
	}()

	//
	// A-Side (Target) - Create Connection
	//
	prod_url, err := url.Parse(h.Target)
	var clientTcpConn net.Conn

	if prod_url.Scheme == "https" {
		clientTcpConn, err = tls.Dial("tcp", prod_url.Host, &tls.Config{InsecureSkipVerify: true})
	} else {
		clientTcpConn, err = net.DialTimeout("tcp", prod_url.Host, time.Duration(time.Duration(*productionTimeout)*time.Second))
	}
	if err != nil {
		log.Error(fmt.Sprintf("(A-Side) Failed to connect to host %s for url prefix", prod_url.Host, h.Target))
		return
	}

	//
	// A-Side (Target) - Handle Request
	//
	clientHttpConn := httputil.NewClientConn(clientTcpConn, nil) // Start a new HTTP connection on it
	defer clientHttpConn.Close()                                 // Close the connection to the server
	if *productionHostRewrite || prod_url.Scheme == "https" {
		productionRequest.Host = prod_url.Host
	}
	log.WithFields(log.Fields{
		"uuid":                  uid,
		"side":                  "A-Side",
		"request_method":        productionRequest.Method,
		"request_path":          productionRequest.URL.Path,
		"request_proto":         productionRequest.Proto,
		"request_host":          productionRequest.Host,
		"request_contentlength": productionRequest.ContentLength,
	}).Info("Proxy Request")
	err = clientHttpConn.Write(productionRequest) // Pass on the request
	if err != nil {
		log.Error(fmt.Sprintf("(A-Side) Failed to send to %s: %v", h.Target, err))
		return
	}
	a_resp, err := clientHttpConn.Read(productionRequest) // Read back the reply
	if err != nil && err != httputil.ErrPersistEOF {
		log.Error(fmt.Sprintf("(A-Side) Failed to receive from %s: %v", h.Target, err))
		return
	}
	log.WithFields(log.Fields{
		"uuid":                   uid,
		"side":                   "B-Side",
		"response_code":          a_resp.StatusCode,
		"response_contentlength": a_resp.ContentLength,
	}).Info("Proxy Response")
	defer a_resp.Body.Close()
	for k, v := range a_resp.Header {
		w.Header()[k] = v
	}
	w.WriteHeader(a_resp.StatusCode)
	body, _ := ioutil.ReadAll(a_resp.Body)
	w.Write(body)
}

func main() {
	flag.Parse()

	log.SetOutput(os.Stdout)
	// Log as JSON instead of the default ASCII formatter
	if *jsonLogging {
		log.SetFormatter(&log.JSONFormatter{})
	}
	// Set appropriate logging level
	switch {
	case *debug == 0:
		log.SetLevel(log.ErrorLevel)
	case *debug == 1:
		log.SetLevel(log.WarnLevel)
	case *debug == 2:
		log.SetLevel(log.InfoLevel)
	case *debug >= 3:
		log.SetLevel(log.DebugLevel)
	}

	runtime.GOMAXPROCS(runtime.NumCPU())

	var err error

	var listener net.Listener

	if len(*tlsPrivateKey) > 0 {
		cer, err := tls.LoadX509KeyPair(*tlsCertificate, *tlsPrivateKey)
		if err != nil {
			log.Error(fmt.Sprintf("Failed to load certficate: %s and private key: %s", *tlsCertificate, *tlsPrivateKey))
			return
		}

		config := &tls.Config{Certificates: []tls.Certificate{cer}}
		listener, err = tls.Listen("tcp", *listen, config)
		if err != nil {
			log.Error(fmt.Sprintf("Failed to listen to (SSL) %s: %s", *listen, err))
			return
		}
	} else {
		listener, err = net.Listen("tcp", *listen)
		if err != nil {
			log.Error(fmt.Sprintf("Failed to listen to %s: %s", *listen, err))
			return
		}
	}

	//
	// This is my being lazy with dealing with inferred default ports and schemes
	// and instead to just force everything to be completely explicit since it was simpler...
	//

	// A-Side (Target) URL Pre-Processing
	u, err := url.Parse(*targetProduction)
	_, u_port, _ := net.SplitHostPort(u.Host)
	if err != nil || len(u.Scheme) == 0 || len(u_port) == 0 {
		log.Error(fmt.Sprintf("Bad format (A) %s - Must be complete URI including scheme & port", *targetProduction))
		return
	}
	// B-Side (Alternate) URL Pre-Processing
	u, err = url.Parse(*altTarget)
	_, u_port, _ = net.SplitHostPort(u.Host)
	if err != nil || len(u.Scheme) == 0 || len(u_port) == 0 {
		log.Error(fmt.Sprintf("Bad format (B) %s - Must be complete URI including scheme & port", *altTarget))
		return
	}

	log.WithFields(log.Fields{
		"proxy_port":    *listen,
		"proxy_percent": *percent,
		"proxy_tls":     len(*tlsPrivateKey) > 0,
		"a_url":         *targetProduction,
		"a_timeout":     *productionTimeout,
		"a_rewrite":     *productionHostRewrite,
		"b_url":         *altTarget,
		"b_timeout":     *alternateTimeout,
		"b_rewrite":     *alternateHostRewrite,
	}).Info("TeeProxy Initializing")

	// At this point we are theoretically good to go
	// Serve & Proxy...
	h := handler{
		Target:      *targetProduction,
		Alternative: *altTarget,
		Randomizer:  *rand.New(rand.NewSource(time.Now().UnixNano())),
	}
	http.Serve(listener, h)
}

type nopCloser struct {
	io.Reader
}

func (nopCloser) Close() error { return nil }

func DuplicateRequest(request *http.Request) (request1 *http.Request, request2 *http.Request) {
	b1 := new(bytes.Buffer)
	b2 := new(bytes.Buffer)
	w := io.MultiWriter(b1, b2)
	io.Copy(w, request.Body)
	defer request.Body.Close()
	request1 = &http.Request{
		Method:        request.Method,
		URL:           request.URL,
		Proto:         request.Proto,
		ProtoMajor:    request.ProtoMajor,
		ProtoMinor:    request.ProtoMinor,
		Header:        request.Header,
		Body:          nopCloser{b1},
		Host:          request.Host,
		ContentLength: request.ContentLength,
		Close:         true,
	}
	request2 = &http.Request{
		Method:        request.Method,
		URL:           request.URL,
		Proto:         request.Proto,
		ProtoMajor:    request.ProtoMajor,
		ProtoMinor:    request.ProtoMinor,
		Header:        request.Header,
		Body:          nopCloser{b2},
		Host:          request.Host,
		ContentLength: request.ContentLength,
		Close:         true,
	}
	return
}
