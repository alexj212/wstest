package main

import (
	"embed"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"github.com/gorilla/sessions"
	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/webrtc/v3"
	"html/template"
	"log"
	"net/http"
	"sync"
	"time"
)

// Embed the contents of the templates and static directories
//
//go:embed templates/*
//go:embed static/*
var content embed.FS
var store = sessions.NewCookieStore([]byte("your-secret-key"))

var (
	publishers  = make(map[string]*Publisher)
	lpublishers sync.Mutex

	viewers  = make(map[string]*Viewer)
	lviewers sync.Mutex
)

func init() {
	// Register custom types with gob
	gob.Register(&Publisher{})
	gob.Register(&Viewer{})
}

type Publisher struct {
	publisherTrack *webrtc.TrackLocalStaticRTP
	trackMutex     sync.Mutex

	peerConnectionPublisher *webrtc.PeerConnection

	// ice for Publisher
	iceCandidatesP           []webrtc.ICECandidateInit
	iceMutexP                sync.Mutex
	pendingRemoteCandidatesP []webrtc.ICECandidateInit // to store early remote candidates coming when remote description is not ready
	remoteCandidatesMtxP     sync.Mutex
	Valid                    bool
	lvalid                   sync.Mutex
}
type Viewer struct {
	peerConnectionViewer *webrtc.PeerConnection
	// ice for Viewer
	iceCandidatesV           []webrtc.ICECandidateInit
	iceMutexV                sync.Mutex
	pendingRemoteCandidatesV []webrtc.ICECandidateInit // to store early remote candidates coming when remote description is not ready
	remoteCandidatesMtxV     sync.Mutex
	valid                    bool
	lvalid                   sync.Mutex
}

// Function to parse the SDP from the request body
func parseSDP(r *http.Request, sdp *webrtc.SessionDescription) error {
	if err := r.ParseForm(); err != nil {
		return err
	}

	sdpType := r.FormValue("type")
	if sdpType != "offer" && sdpType != "answer" {
		return fmt.Errorf("invalid SDP type")
	}

	sdp.SDP = r.FormValue("sdp")
	sdp.Type = webrtc.NewSDPType(sdpType)
	return nil
}

// Watchdog to check Publisher connection status and RTP senders
func startWatchdog() {
	ticker := time.NewTicker(10 * time.Second) // Check every 10 seconds
	go func() {
		for range ticker.C {

			lpublishers.Lock()
			lviewers.Lock()
			log.Printf("Watchdog: %d publishers, %d viewers\n", len(publishers), len(viewers))
			for _, p := range publishers {

				p.trackMutex.Lock()
				if p.peerConnectionPublisher != nil && p.publisherTrack != nil {
					log.Println("Watchdog: Publisher is connected.")
					// Check and log RTP senders and tracks
					senders := p.peerConnectionPublisher.GetSenders()
					if len(senders) > 0 {
						for i, sender := range senders {
							if sender.Track() != nil {
								log.Printf("Watchdog: Sender %d - Kind: %s, Label: %v\n", i+1, sender.Track().Kind(), sender.Track())
							} else {
								log.Printf("Watchdog: Sender %d - No track attached\n", i+1)
							}
						}
					} else {
						log.Println("Watchdog: No senders available.")
					}
				} else {
					log.Println("Watchdog: No Publisher connected.")
				}
				p.trackMutex.Unlock()
			}
			lpublishers.Unlock()
			lviewers.Unlock()
		}
	}()
}

// Handler for the Publisher
func publishHandler(w http.ResponseWriter, r *http.Request) {
	p := lookupPublisher(w, r, "publishHandler", true)

	// Print all cookies to check if session is set
	for _, cookie := range r.Cookies() {
		fmt.Fprintf(w, "Cookie Name: %s, Value: %s\n", cookie.Name, cookie.Value)
	}
	log.Println("/publish: Publisher caller: %s", "publishHandler")

	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		http.Error(w, "Invalid offer", http.StatusBadRequest)
		return
	}
	log.Println("/publish: SDP parsed successfully. SDP Type:", offer.Type.String())

	config := webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{
				URLs: []string{"stun:stun.l.google.com:19302"},
			},
		},
	}

	settingEngine := webrtc.SettingEngine{}
	i := &interceptor.Registry{}

	m := &webrtc.MediaEngine{}
	if err := m.RegisterDefaultCodecs(); err != nil {
		http.Error(w, fmt.Sprintf("err: %v", err), http.StatusBadRequest)
		return
	}

	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		http.Error(w, fmt.Sprintf("err: %v", err), http.StatusBadRequest)
		return
	}

	// This interceptor sends a PLI every 3 seconds, when new peer joins it will get keyframe to start with automatically.
	// on production, need to have PLI logic (keyframe) that will only be requested when new peer joins
	intervalPliFactory, err := intervalpli.NewReceiverInterceptor()
	if err != nil {
		http.Error(w, fmt.Sprintf("err: %v", err), http.StatusBadRequest)
		return
	}
	i.Add(intervalPliFactory)

	// create new peer connection
	peer, err := webrtc.NewAPI(webrtc.WithInterceptorRegistry(i), webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(settingEngine)).NewPeerConnection(config)
	if err != nil {
		log.Println("/publish: Error creating PeerConnection:", err)
		http.Error(w, "Failed to create PeerConnection", http.StatusInternalServerError)
		return
	}
	p.peerConnectionPublisher = peer

	// Create Track that we send video back to browser on
	outputTrack, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "video", "pion")
	if err != nil {
		http.Error(w, fmt.Sprintf("err: %v", err), http.StatusBadRequest)
		return
	}

	// Add this newly created track to the PeerConnection
	rtpSender, err := p.peerConnectionPublisher.AddTrack(outputTrack)
	if err != nil {
		http.Error(w, fmt.Sprintf("err: %v", err), http.StatusBadRequest)
		return
	}

	// Read incoming RTCP packets
	// Before these packets are returned they are processed by interceptors. For things
	// like NACK this needs to be called.
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSender.Read(rtcpBuf); rtcpErr != nil {
				return
			}
		}
	}()

	// Log ICE connection state changes
	p.peerConnectionPublisher.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("/publish: ICE Connection State has changed: %s\n", state.String())
	})

	p.peerConnectionPublisher.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			p.iceMutexP.Lock()
			p.iceCandidatesP = append(p.iceCandidatesP, c.ToJSON())
			p.iceMutexP.Unlock()
		}
	})

	p.peerConnectionPublisher.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Printf("Peer Connection State has changed: %s\n", s.String())

		p.lvalid.Lock()
		defer p.lvalid.Unlock()
		if s == webrtc.PeerConnectionStateConnected {
			fmt.Println("Peer connected")
			p.Valid = true
		}

		if s == webrtc.PeerConnectionStateFailed {
			fmt.Println("Peer Connection has gone to failed exiting")
			p.Valid = false
			return
		}

		if s == webrtc.PeerConnectionStateClosed {
			fmt.Println("Peer Connection has gone to closed exiting")
			p.Valid = false
			return
		}
	})

	// Handle incoming media from the Publisher and log RTP packets
	p.peerConnectionPublisher.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Println("/publish: Received track from Publisher. Kind:", track.Kind(), "SSRC:", track.SSRC())

		p.trackMutex.Lock()
		defer p.trackMutex.Unlock()

		if p.publisherTrack == nil {
			p.publisherTrack, err = webrtc.NewTrackLocalStaticRTP(track.Codec().RTPCodecCapability, "video", "sfu")
			if err != nil {
				log.Println("/publish: Error creating local track:", err)
				return
			}
			log.Println("/publish: Publisher track initialized.")
		}

		// Log RTP packets from the Publisher
		go func() {
			for {
				packet, _, err := track.ReadRTP()
				if err != nil {
					log.Println("/publish: Error reading RTP packet:", err)
					break
				}

				// Log RTP packet details
				//log.Printf("/publish: RTP Packet - SSRC: %d, Sequence: %d, Timestamp: %d, PayloadType: %d\n",
				//packet.SSRC, packet.SequenceNumber, packet.Timestamp, packet.PayloadType)

				// Write the RTP packet to the local Publisher track
				if err := p.publisherTrack.WriteRTP(packet); err != nil {
					log.Println("/publish: Error writing RTP to local track:", err)
					break
				}
			}
		}()
	})

	// Set the remote description
	err = p.peerConnectionPublisher.SetRemoteDescription(offer)
	if err != nil {
		log.Println("/publish: Error setting remote description:", err)
		http.Error(w, "Could not set remote description", http.StatusInternalServerError)
		return
	}
	log.Println("/publish: Remote description set.")

	// Create an answer and send it back
	answer, err := p.peerConnectionPublisher.CreateAnswer(nil)
	if err != nil {
		log.Println("/publish: Error creating answer:", err)
		http.Error(w, "Could not create answer", http.StatusInternalServerError)
		return
	}

	err = p.peerConnectionPublisher.SetLocalDescription(answer)
	if err != nil {
		log.Println("/publish: Error setting local description:", err)
		http.Error(w, "Could not set local description", http.StatusInternalServerError)
		return
	}
	log.Println("/publish: Local description set. Sending SDP answer.")

	// Log the SDP for debugging purposes
	log.Printf("/publish: Sending SDP answer\n")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(answer)

	log.Println("/publish: Publisher process completed.")
}

func lookupViewer(w http.ResponseWriter, r *http.Request) *Viewer {
	session, _ := store.Get(r, "session-id")
	if session.IsNew {
		log.Printf("lookupPublisher: New session created. %s", session.ID)
	} else {
		log.Printf("lookupPublisher: existing. %s", session.ID)
	}
	// Set user as authenticated
	v, ok := session.Values["Viewer"]
	if !ok {

		v = &Viewer{}
		session.Values["Viewer"] = v
		lviewers.Lock()
		viewers[session.ID] = v.(*Viewer)
		lviewers.Unlock()
		log.Printf("lookupViewer: Viewer not found. Creating new Viewer. %s", session.ID)
	}
	session.Save(r, w)

	return v.(*Viewer)
}

func lookupPublisher(w http.ResponseWriter, r *http.Request, caller string, create bool) *Publisher {
	session, _ := store.Get(r, "session-id")

	if session.IsNew {
		log.Printf("lookupPublisher(%s): New session created. %s", caller, session.ID)
	} else {
		log.Printf("lookupPublisher(%s): existing. %s", caller, session.ID)
	}
	// Set user as authenticated
	p, ok := session.Values["Publisher"]
	if !create {
		return p.(*Publisher)
	}
	if !ok {

		p = &Publisher{}
		session.Values["Publisher"] = p
		lpublishers.Lock()
		publishers[session.ID] = p.(*Publisher)
		lpublishers.Unlock()
		log.Printf("lookupPublisher: Publisher not found. Creating new Publisher. %s", session.ID)
	}
	err := session.Save(r, w)
	if err != nil {
		log.Println("lookupPublisher: Error saving session:", err)

	}
	return p.(*Publisher)
}

// Handler for the Viewer
func viewHandler(w http.ResponseWriter, r *http.Request) {

	v := lookupViewer(w, r)

	log.Println("/view: Viewer caller: %s", "viewHandler")

	var offer webrtc.SessionDescription
	if err := json.NewDecoder(r.Body).Decode(&offer); err != nil {
		http.Error(w, "Invalid offer", http.StatusBadRequest)
		return
	}
	log.Println("/view: SDP parsed successfully. SDP Type:", offer.Type.String())

	var err error
	viewPeerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		log.Println("/view: Error creating PeerConnection:", err)
		http.Error(w, "Failed to create PeerConnection", http.StatusInternalServerError)
		return
	}
	v.peerConnectionViewer = viewPeerConnection

	p := randPublisher()
	if p == nil {
		log.Println("/view: No Publisher track available. Viewer cannot connect.")
		http.Error(w, "No Publisher available", http.StatusServiceUnavailable)
		return
	}

	p.trackMutex.Lock()
	if p.publisherTrack == nil {
		log.Println("/view: No Publisher track available. Viewer cannot connect.")
		http.Error(w, "No Publisher available", http.StatusServiceUnavailable)
		p.trackMutex.Unlock()
		return
	}
	log.Println("/view: Publisher track found. Viewer can connect.")
	p.trackMutex.Unlock()

	// Add the Publisher's track to the Viewer's peer connection
	_, err = v.peerConnectionViewer.AddTrack(p.publisherTrack)
	if err != nil {
		log.Println("/view: Error adding Publisher track to Viewer:", err)
		http.Error(w, "Could not add track", http.StatusInternalServerError)
		return
	}
	log.Println("/view: Publisher track added to Viewer connection.")

	v.peerConnectionViewer.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			v.iceMutexV.Lock()
			v.iceCandidatesV = append(v.iceCandidatesV, c.ToJSON())
			v.iceMutexV.Unlock()
		}
	})

	// Log ICE connection state changes
	v.peerConnectionViewer.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("/view: ICE Connection State has changed: %s\n", state.String())
	})

	v.peerConnectionViewer.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Printf("[Viewer] Peer Connection State has changed: %s\n", s.String())

		v.lvalid.Lock()
		defer v.lvalid.Unlock()
		if s == webrtc.PeerConnectionStateConnected {
			fmt.Println("[Viewer] Peer connected")
			v.valid = true
		}

		if s == webrtc.PeerConnectionStateFailed {
			fmt.Println("Peer Connection has gone to failed exiting")
			v.valid = false
			return
		}

		if s == webrtc.PeerConnectionStateClosed {
			fmt.Println("Peer Connection has gone to closed exiting")
			v.valid = false
			return
		}
	})

	// Set the remote description
	err = v.peerConnectionViewer.SetRemoteDescription(offer)
	if err != nil {
		log.Println("/view: Error setting remote description:", err)
		http.Error(w, "Could not set remote description", http.StatusInternalServerError)
		return
	}
	log.Println("/view: Remote description set.")

	// Create an answer and send it back
	answer, err := v.peerConnectionViewer.CreateAnswer(nil)
	if err != nil {
		log.Println("/view: Error creating answer:", err)
		http.Error(w, "Could not create answer", http.StatusInternalServerError)
		return
	}

	err = v.peerConnectionViewer.SetLocalDescription(answer)
	if err != nil {
		log.Println("/view: Error setting local description:", err)
		http.Error(w, "Could not set local description", http.StatusInternalServerError)
		return
	}
	log.Println("/view: Local description set. Sending SDP answer.")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(answer)

	log.Println("/view: Viewer process completed.")
}

func randPublisher() *Publisher {
	for _, p := range publishers {
		if p.Valid {
			return p
		}
	}
	return nil
}

func main() {
	// Start the watchdog
	startWatchdog()

	// Parse the HTML template
	tmpl := template.Must(template.ParseFS(content, "templates/index.html"))

	// Serve the main page with CSP headers
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "script-src 'self';")
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")

		err := tmpl.Execute(w, nil)
		if err != nil {
			log.Println("/: Error rendering template:", err)
			http.Error(w, "Failed to render template", http.StatusInternalServerError)
		} else {
			log.Println("/: Main page served successfully.")
		}
	})

	// Set up the handlers for publishing and viewing streams
	http.HandleFunc("/publish", publishHandler)
	http.HandleFunc("/view", viewHandler)

	// ice for Publisher
	http.HandleFunc("/ice-candidate-p", handleIceCandidatePublisher)
	http.HandleFunc("/ice-candidates-p", handleIceCandidatesPublisher)

	// ice for Viewer
	http.HandleFunc("/ice-candidate-v", handleIceCandidateViewer)
	http.HandleFunc("/ice-candidates-v", handleIceCandidatesViewer)

	// Serve static JavaScript files
	http.Handle("/static/", noCacheHandler(http.FileServer(http.FS(content))))

	// Start the HTTP server
	log.Println("Server running at http://localhost:8080")
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Fatal("Server failed:", err)
	}
}

func noCacheHandler(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Set headers to disable caching
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		h.ServeHTTP(w, r)
	})
}
func handleIceCandidatePublisher(w http.ResponseWriter, r *http.Request) {
	var candidate webrtc.ICECandidateInit
	if err := json.NewDecoder(r.Body).Decode(&candidate); err != nil {
		http.Error(w, "Invalid ICE candidate", http.StatusBadRequest)
		return
	}
	p := lookupPublisher(w, r, "handleIceCandidatePublisher", false)
	if p == nil {
		return
	}
	p.remoteCandidatesMtxP.Lock()
	defer p.remoteCandidatesMtxP.Unlock()

	if p.peerConnectionPublisher == nil {
		return
	}

	desc := p.peerConnectionPublisher.RemoteDescription()
	if desc == nil {
		p.pendingRemoteCandidatesP = append(p.pendingRemoteCandidatesP, candidate)
		return
	}

	if err := p.peerConnectionPublisher.AddICECandidate(candidate); err != nil {
		http.Error(w, "Failed to add ICE candidate", http.StatusInternalServerError)
		return
	}

	//fmt.Println("[publilsher peer] ice candidate", candidate)
}

func handleIceCandidatesPublisher(w http.ResponseWriter, r *http.Request) {
	p := lookupPublisher(w, r, "handleIceCandidatesPublisher", false)
	if p == nil {
		return
	}
	p.iceMutexP.Lock()
	candidates := p.iceCandidatesP
	p.iceCandidatesP = nil
	p.iceMutexP.Unlock()

	if candidates == nil {
		candidates = []webrtc.ICECandidateInit{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(candidates)
}

func handleIceCandidateViewer(w http.ResponseWriter, r *http.Request) {

	v := lookupViewer(w, r)

	var candidate webrtc.ICECandidateInit
	if err := json.NewDecoder(r.Body).Decode(&candidate); err != nil {
		http.Error(w, "Invalid ICE candidate", http.StatusBadRequest)
		return
	}

	v.remoteCandidatesMtxV.Lock()
	defer v.remoteCandidatesMtxV.Unlock()

	if v.peerConnectionViewer == nil {
		return
	}

	desc := v.peerConnectionViewer.RemoteDescription()
	if desc == nil {
		v.pendingRemoteCandidatesV = append(v.pendingRemoteCandidatesV, candidate)
		return
	}

	if err := v.peerConnectionViewer.AddICECandidate(candidate); err != nil {
		http.Error(w, "Failed to add ICE candidate", http.StatusInternalServerError)
		return
	}

	fmt.Println("[Viewer peer] ice candidate", candidate)
}

func handleIceCandidatesViewer(w http.ResponseWriter, r *http.Request) {

	v := lookupViewer(w, r)

	v.iceMutexV.Lock()
	candidates := v.iceCandidatesV
	v.iceCandidatesV = nil
	v.iceMutexV.Unlock()

	if candidates == nil {
		candidates = []webrtc.ICECandidateInit{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(candidates)
}
