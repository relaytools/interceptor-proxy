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
	// the incoming WebSocket connection, along with the host response containing mode and other details.
	// Request is the initial incoming and unmodified request.
	Backend func(*http.Request) (*url.URL, HostResponse)

	// Upgrader specifies the parameters for upgrading a incoming HTTP
	// connection to a WebSocket connection. If nil, DefaultUpgrader is used.
	Upgrader *websocket.Upgrader

	//  Dialer contains options for connecting to the backend WebSocket server.
	//  If nil, DefaultDialer is used.
	Dialer *websocket.Dialer

	//Config URL
	ConfigURL *string
}

type NostrReq struct {
	Kinds   []int    `json:"kinds"`
	Authors []string `json:"authors"`
}

type NostrEvent struct {
}

type HostResponse struct {
	Name   string `json:"name"`
	IP     string `json:"ip"`
	Port   int    `json:"port"`
	Domain string `json:"domain"`
	// Mode can be 'authrequired','authmixed', or 'authnone', or 'authnone:requestpayment'
	Mode    string `json:"mode"`
	Invoice string `json:"invoice"`
}

type AuthorizedResponse struct {
	Authorized bool   `json:"authorized"`
	Status     string `json:"status"`
	Invoice    string `json:"invoice"`
}

func quickHostQuery(hostname string, cURL string) (string, HostResponse, error) {
	url := cURL + "/authrequired?" + "host=" + hostname
	rClient := http.Client{
		Timeout: time.Second * 10,
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)

	if err != nil {
		log.Println(err.Error())
		return "", HostResponse{}, err
	}

	res, getErr := rClient.Do(req)
	if getErr != nil {
		log.Println(getErr.Error())
		return "", HostResponse{}, err
	}

	if res.StatusCode == 200 {
		defer res.Body.Close()
		// Read the raw body
		bodyBytes, err := io.ReadAll(res.Body)
		if err != nil {
			log.Println("Error reading body:", err)
			return "", HostResponse{}, err
		}

		// Print the raw response
		//log.Printf("Raw response body: %s", string(bodyBytes))

		var hostResponse HostResponse
		decodeErr := json.Unmarshal(bodyBytes, &hostResponse)
		if decodeErr != nil {
			log.Printf("JSON decode error: %v for body: %s", decodeErr, string(bodyBytes))
			return "", HostResponse{}, decodeErr
		}

		log.Printf("Decoded hostResponse: %+v", hostResponse)

		useIP := "127.0.0.1"
		if hostResponse.IP != "" {
			useIP = hostResponse.IP
		}

		uri := fmt.Sprintf("ws://%s:%d", useIP, hostResponse.Port)
		log.Println("relay mode is: ", hostResponse.Mode)

		return uri, hostResponse, nil
	} else {
		return "", HostResponse{}, fmt.Errorf("error unmarshaling json")
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

// NewProxy returns a new Websocket reverse proxy that rewrites the
// URL's to the scheme, host and base path provider in target.
// mode can be private_relay or protected_dms
func NewProxy(cURL string) *WebsocketProxy {

	backend := func(r *http.Request) (*url.URL, HostResponse) {
		response, hostResponse, err := quickHostQuery(r.Host, cURL)
		// u is in the format ws://IP:PORT
		if err != nil {
			log.Printf("websocketproxy: couldn't get backend URL %s", response)
			return nil, hostResponse
		}

		u, err := url.Parse(response)
		if err != nil {
			log.Printf("websocketproxy: couldn't parse URL %s", response)
			return nil, hostResponse
		}

		u.Fragment = r.URL.Fragment
		u.Path = r.URL.Path
		u.RawQuery = r.URL.RawQuery
		return u, hostResponse
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

	backendURL, hostResponse := w.Backend(req)
	if backendURL == nil {
		log.Println("websocketproxy: backend URL is nil")
		http.Error(rw, "internal server error (code: 2)", http.StatusInternalServerError)
		return
	}

	log.Printf("Proxying: %s -> %s (mode: %s)", req.Host, backendURL, hostResponse.Mode)

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
		requestHeader.Add("Sec-Websocket-Protocol", prot)
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
	realip := req.Header.Get("X-Real-IP")
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

	authCount := 0
	authComplete := false
	var loggedInAs = make(map[string]string)
	var authStatus string
	var authmessage []byte
	var ev nostr.Event

	// If mode is authnone, skip authentication
	if strings.Contains(hostResponse.Mode, "authnone") {
		authComplete = true
		if hostResponse.Mode == "authnone:requestpayment" {
			// send NOTIFY
			noticeMessage := fmt.Sprintf(`["NOTIFY","This relay is requesting that you consider donating to the relay using the invoice below or visit https://%s for more info.\n%s\nThank you!\n"]`, req.Host, hostResponse.Invoice)
			if err := connPub.WriteMessage(websocket.TextMessage, []byte(noticeMessage)); err != nil {
				log.Printf("websocketproxy: couldn't send notify message: %s", err)
			}
		}
	} else {

		// Send initial AUTH challenge
		if err := connPub.WriteMessage(websocket.TextMessage, authRequest); err != nil {
			log.Printf("websocketproxy: couldn't send initial message: %s", err)
			return
		}

		for !authComplete {
			// Wait for the response
			_, authmessage, err = connPub.ReadMessage()
			if err != nil {
				log.Printf("websocketproxy: couldn't read message: %s", err)
				return
			}

			// tarpit
			if authCount > 40 {
				log.Printf("Client entering tarpit (tries: %d): IP: %s", authCount, realip)
				time.Sleep(60 * time.Second)
			}
			authCount += 1

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
				// send EOSE, this is not in the spec (anymore?) seems that scrapers will want it tho, so they can go away.
				//eoseString := fmt.Sprintf(`["EOSE","%v"]`, result[1])
				//eoseReq := []byte(eoseString)
				//if err := connPub.WriteMessage(websocket.TextMessage, eoseReq); err != nil {
				//	log.Printf("websocketproxy: couldn't send closeReq message: %s", err)
				//	return
				//}
			}

			if result[0] == "EVENT" {
				var rawMessage []json.RawMessage
				json.Unmarshal(authmessage, &rawMessage)
				var ev nostr.Event
				err := json.Unmarshal(rawMessage[1], &ev)
				if err != nil {
					log.Printf("Error unmarshaling json %v", rawMessage[1])
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

			if result[0] == "NEG-OPEN" {
				negErr := fmt.Sprintf(`["NEG-ERR","%v","auth-required: you must auth"]`, result[1])
				closeReq := []byte(negErr)
				if err := connPub.WriteMessage(websocket.TextMessage, closeReq); err != nil {
					log.Printf("websocketproxy: couldn't send closeReq message: %s", err)
					return
				}
			}

			if result[0] == "AUTH" {
				var rawMessage []json.RawMessage
				json.Unmarshal(authmessage, &rawMessage)
				var ev nostr.Event
				err := json.Unmarshal(rawMessage[1], &ev)
				if err != nil {
					log.Printf("Error unmarshaling json %v", rawMessage[1])
					return
				}

				fmt.Println("wss://" + req.Host)
				gotPubkey, gotOk := nip42.ValidateAuthEvent(&ev, challengeString, "wss://"+req.Host)

				successAuth, authResp := quickQuery(req.Host, gotPubkey, *w.ConfigURL)

				if gotOk && successAuth {
					okString := fmt.Sprintf(`["OK","%v",true,""]`, ev.ID)
					okResp := []byte(okString)
					log.Printf("websocketproxy: AUTH success for pubkey %s; %v", gotPubkey, ev.Tags)
					if err := connPub.WriteMessage(websocket.TextMessage, okResp); err != nil {
						log.Printf("websocketproxy: couldn't send AUTH OK message: %s", err)
						return
					}
					authStatus = authResp.Status
					loggedInAs[gotPubkey] = authStatus
					authComplete = true

					fmt.Println("authresponse", authResp)

					if authResp.Invoice != "" {
						// send NOTIFY
						fmt.Println("SENDING AN INVOICE")
						noticeMessage := fmt.Sprintf(`["NOTIFY","This relay is requesting that you consider donating to the relay using the invoice below or visit https://%s for more info.\n%s\nThank you!\n"]`, req.Host, authResp.Invoice)
						if err := connPub.WriteMessage(websocket.TextMessage, []byte(noticeMessage)); err != nil {
							log.Printf("websocketproxy: couldn't send notify message: %s", err)
						}
					}
					continue

				} else {
					okString := fmt.Sprintf(`["OK","%v",false,"restricted: the pubkey does not have access to this relay, or auth event is invalid"]`, ev.ID)
					okResp := []byte(okString)
					log.Printf("websocketproxy: AUTH failed for pubkey: %s", gotPubkey)
					if err := connPub.WriteMessage(websocket.TextMessage, okResp); err != nil {
						log.Printf("websocketproxy: couldn't send AUTH OK message: %s", err)
						return
					}
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
						log.Printf("websocketproxy: Already authenticated, RESENDING OK for %s; %s; %v", ev.PubKey, modifiedMessage, ev.Tags)

						gotNewPubkey, gotNewOk := nip42.ValidateAuthEvent(&ev, challengeString, "wss://"+req.Host)

						if gotNewOk {
							successAuth, authResp := quickQuery(req.Host, gotNewPubkey, *w.ConfigURL)
							if successAuth {
								loggedInAs[gotNewPubkey] = authResp.Status
							}
						}

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
							log.Printf("partial access detected, dropping req for %s", loggedInAs)
							// close the REQ with no results
							closeString := fmt.Sprintf(`["CLOSED","%v","restricted: you have partial access to send DMs, you are not authorized to perform reqs"]`, r[1])
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

				if len(r) > 2 && r[0] == "EVENT" {
					//var ev event.T
					evJson, _ := r[2].(map[string]interface{})
					evKind := evJson["kind"].(float64)
					evPubkey := evJson["pubkey"].(string)

					// if authStatus is partial, the user is only allowed to send Events but not read them.
					// in theory this does not happen because we are blocking all REQs for partially authenticated users already.
					if loggedInAs[evPubkey] == "partial" {
						log.Printf("partial access detected, dropping event for %v", loggedInAs)
						continue
					}

					if evKind == 4 || evKind == 1059 || evKind == 1060 || evKind == 24 || evKind == 25 || evKind == 26 || evKind == 27 || evKind == 35834 {
						/* not necessary only checking that we have authed in some form.
						if loggedInAs[evPubkey] == "" {
							log.Printf("DROPPING SENSITIVE EVENT for unauthenticated user")
							continue
						}*/
						isSensitive = true
						//log.Printf("FOUND PRIVATE EVENT: kind:%0.f, auth:%s, author:%s", evKind, *w.LoggedInAs, evPubkey)
						if loggedInAs[evPubkey] != "" {
							log.Printf("ALLOWING PRIVATE EVENT for author %s, kind %.0f", loggedInAs, evKind)
							isAllow = true
						}
						tags := evJson["tags"].([]interface{})
						for _, tag := range tags {
							tagKey := tag.([]interface{})[0].(string)
							tagVal := tag.([]interface{})[1].(string)
							log.Printf("TAG: %s, %s", tagKey, tagVal)
							if tagKey == "p" && loggedInAs[tagVal] != "" {
								log.Printf("ALLOWING PRIVATE EVENT for ptag %s, kind %.0f", loggedInAs, evKind)
								isAllow = true
							}
						}
					}
					// drop this message if it's sensitive and didn't contain the P tag for logged in pubkey
					if isSensitive && !isAllow {
						log.Printf("DROPPING PRIVATE EVENT (unauthorized) for %s", loggedInAs)
						continue
					}
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
