// Package websocketproxy is a reverse proxy for WebSocket connections.
package websocketproxy

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/nbd-wtf/go-nostr"
	"github.com/nbd-wtf/go-nostr/nip42"
)

var (
	// DefaultUpgrader specifies the parameters for upgrading an HTTP
	// connection to a WebSocket connection.
	DefaultUpgrader = &websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	// DefaultDialer is a dialer with all fields set to the default zero values.
	DefaultDialer = websocket.DefaultDialer
)

// WebsocketProxy is an HTTP Handler that takes an incoming WebSocket

// connection and proxies it to another server.
type WebsocketProxy struct {
	// Director, if non-nil, is a function that may copy additional request
	// headers from the incoming WebSocket connection into the output headers
	// which will be forwarded to another server.
	Director func(incoming *http.Request, out http.Header)

	// Backend returns the backend URL which the proxy uses to reverse proxy
	// the incoming WebSocket connection. Request is the initial incoming and
	// unmodified request.
	Backend func(*http.Request) *url.URL

	// Upgrader specifies the parameters for upgrading a incoming HTTP
	// connection to a WebSocket connection. If nil, DefaultUpgrader is used.
	Upgrader *websocket.Upgrader

	//  Dialer contains options for connecting to the backend WebSocket server.
	//  If nil, DefaultDialer is used.
	Dialer *websocket.Dialer

	//Logged in as (pubkey)
	LoggedInAs *string

	//Config URL
	ConfigURL *string
}

type NostrReq struct {
	Kinds []int `json:"kinds"`
	Authors []string `json:"authors"`
}

type NostrEvent struct {

}

type HostResponse struct {
	Name string `json:"name"`
	IP string `json:"ip"`
	Port int `json:"port"`
	Domain string `json:"domain"`
}

type AuthorizedResponse struct {
	Authorized bool `json:"authorized"`
	Status string `json:"status"`
}

func quickHostQuery(hostname string, cURL string) (string, error) {
	url := cURL + "/authrequired?" + "host=" + hostname
	rClient := http.Client{
		Timeout: time.Second * 10,
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)

	if err != nil {
		log.Println(err.Error())
		return "", err
	}

	res, getErr := rClient.Do(req)
	if getErr != nil {
		log.Println(getErr.Error())
		return "", err
	}

	if res.StatusCode == 200 {
		defer res.Body.Close()
		var hostResponse HostResponse
        decodeErr := json.NewDecoder(res.Body).Decode(&hostResponse)

        if decodeErr != nil {
            log.Println(decodeErr.Error())
            return "", decodeErr
        }

		useIP := "127.0.0.1"
		if hostResponse.IP != "" {
			useIP = hostResponse.IP
		}

		uri := fmt.Sprintf("ws://%s:%d", useIP, hostResponse.Port)

        return uri, nil
	} else {
		return "", fmt.Errorf("error unmarshaling json")
	}
}

func quickQuery(hostname string, pubkey string, cURL string) (bool, AuthorizedResponse) {
	var authResponse AuthorizedResponse

	log.Printf("quickQuery: %s %s", hostname, pubkey)
	url := cURL + "/authorized?" + "host=" + hostname + "&pubkey=" + pubkey
	rClient := http.Client{
		Timeout: time.Second * 10,
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)

	if err != nil {
		log.Println(err.Error())
		return false, authResponse
	}

	res, getErr := rClient.Do(req)
	if getErr != nil {
		log.Println(getErr.Error())
		return false, authResponse
	}

	if res.StatusCode == 200 {
		defer res.Body.Close()
		decodeErr := json.NewDecoder(res.Body).Decode(&authResponse)
		if decodeErr != nil {
			log.Println(decodeErr.Error())
			return false, authResponse
		}
		return authResponse.Authorized, authResponse
	} else {
		return false, authResponse
	}
}

// ProxyHandler returns a new http.Handler interface that reverse proxies the
// request to the given target.
//func ProxyHandler(target *url.URL) http.Handler { return NewProxy() }

// NewProxy returns a new Websocket reverse proxy that rewrites the
// URL's to the scheme, host and base path provider in target.
// mode can be private_relay or protected_dms
func NewProxy(cURL string) *WebsocketProxy {

	backend := func(r *http.Request) *url.URL {
		// u is in the format ws://IP:PORT
		response, err := quickHostQuery(r.Host, cURL)
		if err != nil {
			log.Printf("websocketproxy: couldn't get backend URL %s", response)
		}

		u, err := url.Parse(response)
		if err != nil {
			log.Printf("websocketproxy: couldn't parse URL %s", response)
		}

		log.Printf("Proxying: %s -> %s", r.Host, u)

		u.Fragment = r.URL.Fragment
		u.Path = r.URL.Path
		u.RawQuery = r.URL.RawQuery
		return u
	}
	
	return &WebsocketProxy{Backend: backend, ConfigURL: &cURL}
}

// ServeHTTP implements the http.Handler that proxies WebSocket connections.
func (w *WebsocketProxy) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	if w.Backend == nil {
		log.Println("websocketproxy: backend function is not defined")
		http.Error(rw, "internal server error (code: 1)", http.StatusInternalServerError)
		return
	}

	backendURL := w.Backend(req)
	if backendURL == nil {
		log.Println("websocketproxy: backend URL is nil")
		http.Error(rw, "internal server error (code: 2)", http.StatusInternalServerError)
		return
	}

	dialer := w.Dialer
	if w.Dialer == nil {
		dialer = DefaultDialer
	}

	// Pass headers from the incoming request to the dialer to forward them to
	// the final destinations.
	requestHeader := http.Header{}
	if origin := req.Header.Get("Origin"); origin != "" {
		requestHeader.Add("Origin", origin)
	}
	for _, prot := range req.Header[http.CanonicalHeaderKey("Sec-WebSocket-Protocol")] {
		requestHeader.Add("Sec-WebSocket-Protocol", prot)
	}
	for _, cookie := range req.Header[http.CanonicalHeaderKey("Cookie")] {
		requestHeader.Add("Cookie", cookie)
	}
	if req.Host != "" {
		requestHeader.Set("Host", req.Host)
	}

	// Pass X-Forwarded-For headers too, code below is a part of
	// httputil.ReverseProxy. See http://en.wikipedia.org/wiki/X-Forwarded-For
	// for more information
	// TODO: use RFC7239 http://tools.ietf.org/html/rfc7239
	if clientIP, _, err := net.SplitHostPort(req.RemoteAddr); err == nil {
		// If we aren't the first proxy retain prior
		// X-Forwarded-For information as a comma+space
		// separated list and fold multiple headers into one.
		if prior, ok := req.Header["X-Forwarded-For"]; ok {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		requestHeader.Set("X-Forwarded-For", clientIP)
	}

	// pass through x-real-ip
	realip := req.Header.Get("X-Real-IP");
	if realip != "" {
		requestHeader.Set("X-Real-IP", realip)
	}

	// Set the originating protocol of the incoming HTTP request. The SSL might
	// be terminated on our site and because we doing proxy adding this would
	// be helpful for applications on the backend.
	requestHeader.Set("X-Forwarded-Proto", "http")
	if req.TLS != nil {
		requestHeader.Set("X-Forwarded-Proto", "https")
	}

	// Enable the director to copy any additional headers it desires for
	// forwarding to the remote server.
	if w.Director != nil {
		w.Director(req, requestHeader)
	}

	// Connect to the backend URL, also pass the headers we get from the requst
	// together with the Forwarded headers we prepared above.
	// TODO: support multiplexing on the same backend connection instead of
	// opening a new TCP connection time for each request. This should be
	// optional:
	// http://tools.ietf.org/html/draft-ietf-hybi-websocket-multiplexing-01
	connBackend, resp, err := dialer.Dial(backendURL.String(), requestHeader)
	if err != nil {
		log.Printf("websocketproxy: couldn't dial to remote backend url %s", err)
		if resp != nil {
			// If the WebSocket handshake fails, ErrBadHandshake is returned
			// along with a non-nil *http.Response so that callers can handle
			// redirects, authentication, etcetera.
			if err := copyResponse(rw, resp); err != nil {
				log.Printf("websocketproxy: couldn't write response after failed remote backend handshake: %s", err)
			}
		} else {
			http.Error(rw, http.StatusText(http.StatusServiceUnavailable), http.StatusServiceUnavailable)
		}
		return
	}
	defer connBackend.Close()

	upgrader := w.Upgrader
	if w.Upgrader == nil {
		upgrader = DefaultUpgrader
	}

	// Only pass those headers to the upgrader.
	upgradeHeader := http.Header{}
	if hdr := resp.Header.Get("Sec-Websocket-Protocol"); hdr != "" {
		upgradeHeader.Set("Sec-Websocket-Protocol", hdr)
	}
	if hdr := resp.Header.Get("Set-Cookie"); hdr != "" {
		upgradeHeader.Set("Set-Cookie", hdr)
	}

	// Now upgrade the existing incoming request to a WebSocket connection.
	// Also pass the header that we gathered from the Dial handshake.
	connPub, err := upgrader.Upgrade(rw, req, upgradeHeader)
	if err != nil {
		log.Printf("websocketproxy: couldn't upgrade %s", err)
		return
	}
	defer connPub.Close()

	// generate challenge using random uuid
	challengeString := uuid.New().String()

	// Send initial message
	initialAuthString := fmt.Sprintf(`["AUTH","%v"]`, challengeString)
	authRequest := []byte(initialAuthString)
	if err := connPub.WriteMessage(websocket.TextMessage, authRequest); err != nil {
		log.Printf("websocketproxy: couldn't send initial message: %s", err)
		return
	}

	// Perform NIP-42 authentication
	authComplete := false
	var authmessage []byte
	var ev nostr.Event
	authStatus := ""
	for !authComplete {
		// Wait for the response
		_, authmessage, err = connPub.ReadMessage()
		if err != nil {
			log.Printf("websocketproxy: couldn't read message: %s", err)
			return
		}

		var result []string
		json.Unmarshal([]byte(authmessage), &result)

		// avoid panic
		if len(result) == 0 {
			continue
		}


		if result[0] == "REQ" {
			if err := connPub.WriteMessage(websocket.TextMessage, authRequest); err != nil {
				log.Printf("websocketproxy: couldn't send initial message: %s", err)
				return
			}
			log.Printf("websocketproxy: received REQ message %s from %s: closing and responding with AUTH", result, realip)
			closeString := fmt.Sprintf(`["CLOSED","%v","auth-required: you must auth"]`, result[1])
			closeReq := []byte(closeString)
			if err := connPub.WriteMessage(websocket.TextMessage, closeReq); err != nil {
				log.Printf("websocketproxy: couldn't send closeReq message: %s", err)
				return
			}
			/* don't send EOSE, this is not in the spec (anymore?)
			eoseString := fmt.Sprintf(`["EOSE","%v"]`, result[1])
			eoseReq := []byte(eoseString)
			if err := connPub.WriteMessage(websocket.TextMessage, eoseReq); err != nil {
				log.Printf("websocketproxy: couldn't send closeReq message: %s", err)
				return
			}
			*/
		}

		if result[0] == "EVENT" {
			// need to get the eventID
			modifiedMessage := authmessage[9 : len(authmessage)-1]

			err := json.Unmarshal(modifiedMessage, &ev)
			if err != nil {
				log.Printf("websocketproxy: couldn't parse event: %s\n, %s\n, %s\n", string(authmessage), string(modifiedMessage), err)
				return
			}

			// send auth, and OK false in response
			if err := connPub.WriteMessage(websocket.TextMessage, authRequest); err != nil {
				log.Printf("websocketproxy: couldn't send initial message: %s", err)
				return
			}
			log.Printf("websocketproxy: received EVENT message %s from %s: responding OK false auth required", result, realip)
			falseString := fmt.Sprintf(`["OK","%s",false,"auth-required: you must auth"]`, ev.ID)
			sendFalse := []byte(falseString)
			if err := connPub.WriteMessage(websocket.TextMessage, sendFalse); err != nil {
				log.Printf("websocketproxy: couldn't send OK=false message: %s", err)
				return
			}
		}

		// Accept any attempt by client to AUTH and allow them through
		if result[0] == "AUTH" {
			//log.Printf("websocketproxy: received AUTH message: %s", authmessage)
			modifiedMessage := authmessage[8 : len(authmessage)-1]

			// parse the event
			//fmt.Println(string(modifiedMessage))
			json.Unmarshal(modifiedMessage, &ev)

			fmt.Println("wss://" + req.Host)
			gotPubkey, gotOk := nip42.ValidateAuthEvent(&ev, challengeString, "wss://" + req.Host)

			successAuth, authResp := quickQuery(req.Host, gotPubkey, *w.ConfigURL)

			if gotOk && successAuth {
				okString := fmt.Sprintf(`["OK","%v",true,""]`, ev.ID)
				okResp := []byte(okString)
				log.Printf("websocketproxy: AUTH success for pubkey %s; %s; %v", gotPubkey, modifiedMessage, ev.Tags)
				if err := connPub.WriteMessage(websocket.TextMessage, okResp); err != nil {
					log.Printf("websocketproxy: couldn't send AUTH OK message: %s", err)
					return
				}
				w.LoggedInAs = &gotPubkey
				authStatus = authResp.Status
				authComplete = true
			} else {
				okString := fmt.Sprintf(`["OK","%v",false,"auth-required: invalid auth received"]`, ev.ID)
				okResp := []byte(okString)
				log.Printf("websocketproxy: AUTH failed for pubkey: %s", gotPubkey)
				if err := connPub.WriteMessage(websocket.TextMessage, okResp); err != nil {
					log.Printf("websocketproxy: couldn't send AUTH OK message: %s", err)
					return
				}
			}
		} 
	}

	errClient := make(chan error, 1)
	errBackend := make(chan error, 1)
	replicateWebsocketConn := func(filter bool, dst, src *websocket.Conn, errc chan error) {
		for {
			msgType, msg, err := src.ReadMessage()

			if err != nil {
				m := websocket.FormatCloseMessage(websocket.CloseNormalClosure, fmt.Sprintf("%v", err))
				if e, ok := err.(*websocket.CloseError); ok {
					if e.Code != websocket.CloseNoStatusReceived {
						m = websocket.FormatCloseMessage(e.Code, e.Text)
					}
				}
				errc <- err
				dst.WriteMessage(websocket.CloseMessage, m)
				break
			}

			// If the connection is already authenticated, respond with OK and continue (or strfry will respond with error)
			if filter == false {
				var r []string
				json.Unmarshal([]byte(msg), &r)
				if len(r) > 0 {
					if r[0] == "AUTH" {
						modifiedMessage := msg[8 : len(msg)-1]
						json.Unmarshal(modifiedMessage, &ev)
						log.Printf("websocketproxy: Already authenticated, RESENDING OK for %s; %s; %v", ev.PubKey, modifiedMessage, ev.Tags)
						okString := fmt.Sprintf(`["OK","%v",true,""]`, ev.ID)
						okResp := []byte(okString)
						if err := connPub.WriteMessage(websocket.TextMessage, okResp); err != nil {
							log.Printf("websocketproxy: couldn't send AUTH OK message: %s", err)
							return
						}	
						continue
					// this is here to handle the case for private inbox relays where we do not allow REQs for partially authenticated users
					} else if r[0] == "REQ" {
						if authStatus == "partial" {
							log.Printf("partial access detected, dropping req for %s", *w.LoggedInAs)
							// close the REQ with no results
							closeString := fmt.Sprintf(`["CLOSED","%v","auth-required: you are not authorized to perform reqs"]`, r[1])
							closeReq := []byte(closeString)
							if err := connPub.WriteMessage(websocket.TextMessage, closeReq); err != nil {
								log.Printf("websocketproxy: couldn't send closeReq message: %s", err)
								return
							}
							// send EOSE
							eoseString := fmt.Sprintf(`["EOSE","%v"]`, r[1])
							eoseReq := []byte(eoseString)
							if err := connPub.WriteMessage(websocket.TextMessage, eoseReq); err != nil {
								log.Printf("websocketproxy: couldn't send closeReq message: %s", err)
								return
							}
							continue
						}
					}
				}
			}

			// filter the returned events, and drop any that are the sensitive types
			// DONE: we only want to run this filter, on one side of the interceptor connection..
			if filter == true {
				var r []interface{}

				isAllow := false
				isSensitive := false

				json.Unmarshal([]byte(msg), &r)

				if len(r) > 2 && r[0] == "EVENT"  {
					//var ev event.T
					evJson, _ := r[2].(map[string]interface{})
					evKind := evJson["kind"].(float64)
					evPubkey := evJson["pubkey"].(string)

					// if authStatus is partial, the user is only allowed to send Events but not read them.
					// in theory this does not happen because we are blocking all REQs for partially authenticated users already.
					if authStatus == "partial" {
						log.Printf("partial access detected, dropping event for %s", *w.LoggedInAs)
						continue
					}

					if evKind == 4 || evKind == 1059 || evKind == 1060 {
						isSensitive = true
						//log.Printf("FOUND PRIVATE EVENT: kind:%0.f, auth:%s, author:%s", evKind, *w.LoggedInAs, evPubkey)
						if evPubkey == *w.LoggedInAs {
							log.Printf("ALLOWING PRIVATE EVENT for author %s, kind %.0f", *w.LoggedInAs, evKind)
							isAllow = true
						}
						tags := evJson["tags"].([]interface{})
						for _, tag := range tags {
							tagKey := tag.([]interface{})[0].(string)
							tagVal := tag.([]interface{})[1].(string)
							//log.Printf("TAG: %s, %s", tagKey, tagVal)
							if(tagKey == "p" && tagVal == *w.LoggedInAs) {
								log.Printf("ALLOWING PRIVATE EVENT for ptag %s, kind %.0f", *w.LoggedInAs, evKind)
								isAllow = true
							}
						}
					}
				}

				// drop this message if it's sensitive and didn't contain the P tag for logged in pubkey
				if isSensitive && !isAllow {
					log.Printf("DROPPING PRIVATE EVENT (unauthorized) for %s", *w.LoggedInAs)
					continue
				}
			}
	
			err = dst.WriteMessage(msgType, msg)
			if err != nil {
				errc <- err
				break
			}
		}
	}

	go replicateWebsocketConn(true, connPub, connBackend, errClient)
	go replicateWebsocketConn(false, connBackend, connPub, errBackend)

	var message string
	select {
	case err = <-errClient:
		message = "websocketproxy: Error when copying from backend to client: %v"
	case err = <-errBackend:
		message = "websocketproxy: Error when copying from client to backend: %v"

	}
	if e, ok := err.(*websocket.CloseError); !ok || e.Code == websocket.CloseAbnormalClosure {
		log.Printf(message, err)
	}
}

func copyHeader(dst, src http.Header) {
	for k, vv := range src {
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

func copyResponse(rw http.ResponseWriter, resp *http.Response) error {
	copyHeader(rw.Header(), resp.Header)
	rw.WriteHeader(resp.StatusCode)
	defer resp.Body.Close()

	_, err := io.Copy(rw, resp.Body)
	return err
}
