package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/intervalpli"
	"github.com/pion/webrtc/v3"
)

// Embed the contents of the templates and static directories
//
//go:embed templates/*
//go:embed static/*
var content embed.FS

var (
	publisherTrack *webrtc.TrackLocalStaticRTP
	trackMutex     sync.Mutex

	peerConnectionPublisher *webrtc.PeerConnection
	peerConnectionViewer    *webrtc.PeerConnection

	// ice for publisher
	iceCandidatesP           = make([]webrtc.ICECandidateInit, 0)
	iceMutexP                sync.Mutex
	pendingRemoteCandidatesP []webrtc.ICECandidateInit // to store early remote candidates coming when remote description is not ready
	remoteCandidatesMtxP     sync.Mutex

	// ice for viewer
	iceCandidatesV           = make([]webrtc.ICECandidateInit, 0)
	iceMutexV                sync.Mutex
	pendingRemoteCandidatesV []webrtc.ICECandidateInit // to store early remote candidates coming when remote description is not ready
	remoteCandidatesMtxV     sync.Mutex
)

type P struct {
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

// Watchdog to check publisher connection status and RTP senders
func startWatchdog() {
	ticker := time.NewTicker(10 * time.Second) // Check every 10 seconds
	go func() {
		for range ticker.C {
			trackMutex.Lock()
			if peerConnectionPublisher != nil && publisherTrack != nil {
				log.Println("Watchdog: Publisher is connected.")
				// Check and log RTP senders and tracks
				senders := peerConnectionPublisher.GetSenders()
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
				log.Println("Watchdog: No publisher connected.")
			}
			trackMutex.Unlock()
		}
	}()
}

// Handler for the publisher
func publishHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("/publish: Publisher connection initiated.")

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
		panic(err)
	}

	if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
		panic(err)
	}

	// This interceptor sends a PLI every 3 seconds, when new peer joins it will get keyframe to start with automatically.
	// on production, need to have PLI logic (keyframe) that will only be requested when new peer joins
	intervalPliFactory, err := intervalpli.NewReceiverInterceptor()
	if err != nil {
		panic(err)
	}
	i.Add(intervalPliFactory)

	// create new peer connection
	p, err := webrtc.NewAPI(webrtc.WithInterceptorRegistry(i), webrtc.WithMediaEngine(m), webrtc.WithSettingEngine(settingEngine)).NewPeerConnection(config)
	if err != nil {
		log.Println("/publish: Error creating PeerConnection:", err)
		http.Error(w, "Failed to create PeerConnection", http.StatusInternalServerError)
		return
	}
	peerConnectionPublisher = p

	// Create Track that we send video back to browser on
	outputTrack, err := webrtc.NewTrackLocalStaticRTP(webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8}, "video", "pion")
	if err != nil {
		panic(err)
	}

	// Add this newly created track to the PeerConnection
	rtpSender, err := peerConnectionPublisher.AddTrack(outputTrack)
	if err != nil {
		panic(err)
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
	peerConnectionPublisher.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("/publish: ICE Connection State has changed: %s\n", state.String())
	})

	peerConnectionPublisher.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			iceMutexP.Lock()
			iceCandidatesP = append(iceCandidatesP, c.ToJSON())
			iceMutexP.Unlock()
		}
	})

	peerConnectionPublisher.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Printf("Peer Connection State has changed: %s\n", s.String())

		if s == webrtc.PeerConnectionStateConnected {
			fmt.Println("Peer connected")
		}

		if s == webrtc.PeerConnectionStateFailed {
			fmt.Println("Peer Connection has gone to failed exiting")
			os.Exit(0)
		}

		if s == webrtc.PeerConnectionStateClosed {
			fmt.Println("Peer Connection has gone to closed exiting")
			os.Exit(0)
		}
	})

	// Handle incoming media from the publisher and log RTP packets
	peerConnectionPublisher.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
		log.Println("/publish: Received track from publisher. Kind:", track.Kind(), "SSRC:", track.SSRC())

		trackMutex.Lock()
		defer trackMutex.Unlock()

		if publisherTrack == nil {
			publisherTrack, err = webrtc.NewTrackLocalStaticRTP(track.Codec().RTPCodecCapability, "video", "sfu")
			if err != nil {
				log.Println("/publish: Error creating local track:", err)
				return
			}
			log.Println("/publish: Publisher track initialized.")
		}

		// Log RTP packets from the publisher
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

				// Write the RTP packet to the local publisher track
				if err := publisherTrack.WriteRTP(packet); err != nil {
					log.Println("/publish: Error writing RTP to local track:", err)
					break
				}
			}
		}()
	})

	// Set the remote description
	err = peerConnectionPublisher.SetRemoteDescription(offer)
	if err != nil {
		log.Println("/publish: Error setting remote description:", err)
		http.Error(w, "Could not set remote description", http.StatusInternalServerError)
		return
	}
	log.Println("/publish: Remote description set.")

	// Create an answer and send it back
	answer, err := peerConnectionPublisher.CreateAnswer(nil)
	if err != nil {
		log.Println("/publish: Error creating answer:", err)
		http.Error(w, "Could not create answer", http.StatusInternalServerError)
		return
	}

	err = peerConnectionPublisher.SetLocalDescription(answer)
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

// Handler for the viewer
func viewHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("/view: Viewer connection initiated.")

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
	peerConnectionViewer = viewPeerConnection

	trackMutex.Lock()
	if publisherTrack == nil {
		log.Println("/view: No publisher track available. Viewer cannot connect.")
		http.Error(w, "No publisher available", http.StatusServiceUnavailable)
		trackMutex.Unlock()
		return
	}
	log.Println("/view: Publisher track found. Viewer can connect.")
	trackMutex.Unlock()

	// Add the publisher's track to the viewer's peer connection
	_, err = peerConnectionViewer.AddTrack(publisherTrack)
	if err != nil {
		log.Println("/view: Error adding publisher track to viewer:", err)
		http.Error(w, "Could not add track", http.StatusInternalServerError)
		return
	}
	log.Println("/view: Publisher track added to viewer connection.")

	peerConnectionViewer.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			iceMutexV.Lock()
			iceCandidatesV = append(iceCandidatesV, c.ToJSON())
			iceMutexV.Unlock()
		}
	})

	// Log ICE connection state changes
	peerConnectionViewer.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("/view: ICE Connection State has changed: %s\n", state.String())
	})

	peerConnectionViewer.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		fmt.Printf("[viewer] Peer Connection State has changed: %s\n", s.String())

		if s == webrtc.PeerConnectionStateConnected {
			fmt.Println("[viewer] Peer connected")
		}

		if s == webrtc.PeerConnectionStateFailed {
			fmt.Println("Peer Connection has gone to failed exiting")
			os.Exit(0)
		}

		if s == webrtc.PeerConnectionStateClosed {
			fmt.Println("Peer Connection has gone to closed exiting")
			os.Exit(0)
		}
	})

	// Set the remote description
	err = peerConnectionViewer.SetRemoteDescription(offer)
	if err != nil {
		log.Println("/view: Error setting remote description:", err)
		http.Error(w, "Could not set remote description", http.StatusInternalServerError)
		return
	}
	log.Println("/view: Remote description set.")

	// Create an answer and send it back
	answer, err := peerConnectionViewer.CreateAnswer(nil)
	if err != nil {
		log.Println("/view: Error creating answer:", err)
		http.Error(w, "Could not create answer", http.StatusInternalServerError)
		return
	}

	err = peerConnectionViewer.SetLocalDescription(answer)
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

func main() {
	// Start the watchdog
	startWatchdog()

	// Parse the HTML template
	tmpl := template.Must(template.ParseFS(content, "templates/index.html"))

	// Serve the main page with CSP headers
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "script-src 'self';")
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

	// ice for publisher
	http.HandleFunc("/ice-candidate-p", handleIceCandidatePublisher)
	http.HandleFunc("/ice-candidates-p", handleIceCandidatesPublisher)

	// ice for viewer
	http.HandleFunc("/ice-candidate-v", handleIceCandidateViewer)
	http.HandleFunc("/ice-candidates-v", handleIceCandidatesViewer)

	// Serve static JavaScript files
	http.Handle("/static/", http.FileServer(http.FS(content)))

	// Start the HTTP server
	log.Println("Server running at http://localhost:8080")
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Fatal("Server failed:", err)
	}
}

func handleIceCandidatePublisher(w http.ResponseWriter, r *http.Request) {
	var candidate webrtc.ICECandidateInit
	if err := json.NewDecoder(r.Body).Decode(&candidate); err != nil {
		http.Error(w, "Invalid ICE candidate", http.StatusBadRequest)
		return
	}

	remoteCandidatesMtxP.Lock()
	defer remoteCandidatesMtxP.Unlock()

	if peerConnectionPublisher == nil {
		return
	}

	desc := peerConnectionPublisher.RemoteDescription()
	if desc == nil {
		pendingRemoteCandidatesP = append(pendingRemoteCandidatesP, candidate)
		return
	}

	if err := peerConnectionPublisher.AddICECandidate(candidate); err != nil {
		http.Error(w, "Failed to add ICE candidate", http.StatusInternalServerError)
		return
	}

	//fmt.Println("[publilsher peer] ice candidate", candidate)
}

func handleIceCandidatesPublisher(w http.ResponseWriter, r *http.Request) {
	iceMutexP.Lock()
	candidates := iceCandidatesP
	iceCandidatesP = nil
	iceMutexP.Unlock()

	if candidates == nil {
		candidates = []webrtc.ICECandidateInit{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(candidates)
}

func handleIceCandidateViewer(w http.ResponseWriter, r *http.Request) {
	var candidate webrtc.ICECandidateInit
	if err := json.NewDecoder(r.Body).Decode(&candidate); err != nil {
		http.Error(w, "Invalid ICE candidate", http.StatusBadRequest)
		return
	}

	remoteCandidatesMtxV.Lock()
	defer remoteCandidatesMtxV.Unlock()

	if peerConnectionViewer == nil {
		return
	}

	desc := peerConnectionViewer.RemoteDescription()
	if desc == nil {
		pendingRemoteCandidatesV = append(pendingRemoteCandidatesV, candidate)
		return
	}

	if err := peerConnectionViewer.AddICECandidate(candidate); err != nil {
		http.Error(w, "Failed to add ICE candidate", http.StatusInternalServerError)
		return
	}

	fmt.Println("[viewer peer] ice candidate", candidate)
}

func handleIceCandidatesViewer(w http.ResponseWriter, r *http.Request) {
	iceMutexV.Lock()
	candidates := iceCandidatesV
	iceCandidatesV = nil
	iceMutexV.Unlock()

	if candidates == nil {
		candidates = []webrtc.ICECandidateInit{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(candidates)
}
