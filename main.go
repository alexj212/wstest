package main

import (
	"embed"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"sync"
	"time"

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
	peerConnection *webrtc.PeerConnection
)

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
			if peerConnection != nil && publisherTrack != nil {
				log.Println("Watchdog: Publisher is connected.")
				// Check and log RTP senders and tracks
				senders := peerConnection.GetSenders()
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

	var err error
	peerConnection, err = webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		log.Println("/publish: Error creating PeerConnection:", err)
		http.Error(w, "Failed to create PeerConnection", http.StatusInternalServerError)
		return
	}

	// Log ICE connection state changes
	peerConnection.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("/publish: ICE Connection State has changed: %s\n", state.String())
	})

	// Handle ICE candidates
	peerConnection.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			log.Println("/publish: Received ICE candidate from publisher:", c.ToJSON())
		} else {
			log.Println("/publish: All ICE candidates for publisher have been sent.")
		}
	})

	// Handle incoming media from the publisher and log RTP packets
	peerConnection.OnTrack(func(track *webrtc.TrackRemote, receiver *webrtc.RTPReceiver) {
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
				log.Printf("/publish: RTP Packet - SSRC: %d, Sequence: %d, Timestamp: %d, PayloadType: %d\n",
					packet.SSRC, packet.SequenceNumber, packet.Timestamp, packet.PayloadType)

				// Write the RTP packet to the local publisher track
				if err := publisherTrack.WriteRTP(packet); err != nil {
					log.Println("/publish: Error writing RTP to local track:", err)
					break
				}
			}
		}()
	})

	// Parse the SDP from the request
	offer := webrtc.SessionDescription{}
	err = parseSDP(r, &offer)
	if err != nil {
		log.Println("/publish: Invalid SDP received:", err)
		http.Error(w, "Invalid SDP", http.StatusBadRequest)
		return
	}
	log.Println("/publish: SDP parsed successfully. SDP Type:", offer.Type.String())

	// Set the remote description
	err = peerConnection.SetRemoteDescription(offer)
	if err != nil {
		log.Println("/publish: Error setting remote description:", err)
		http.Error(w, "Could not set remote description", http.StatusInternalServerError)
		return
	}
	log.Println("/publish: Remote description set.")

	// Create an answer and send it back
	answer, err := peerConnection.CreateAnswer(nil)
	if err != nil {
		log.Println("/publish: Error creating answer:", err)
		http.Error(w, "Could not create answer", http.StatusInternalServerError)
		return
	}

	err = peerConnection.SetLocalDescription(answer)
	if err != nil {
		log.Println("/publish: Error setting local description:", err)
		http.Error(w, "Could not set local description", http.StatusInternalServerError)
		return
	}
	log.Println("/publish: Local description set. Sending SDP answer.")

	// Log the SDP for debugging purposes
	log.Printf("/publish: Sending SDP answer\n")
	w.Write([]byte(answer.SDP))
	log.Println("/publish: Publisher process completed.")
}

// Handler for the viewer
func viewHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("/view: Viewer connection initiated.")

	var err error
	viewPeerConnection, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		log.Println("/view: Error creating PeerConnection:", err)
		http.Error(w, "Failed to create PeerConnection", http.StatusInternalServerError)
		return
	}

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
	_, err = viewPeerConnection.AddTrack(publisherTrack)
	if err != nil {
		log.Println("/view: Error adding publisher track to viewer:", err)
		http.Error(w, "Could not add track", http.StatusInternalServerError)
		return
	}
	log.Println("/view: Publisher track added to viewer connection.")

	// Handle ICE candidates
	viewPeerConnection.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			log.Println("/view: Received ICE candidate from viewer:", c.ToJSON())
		} else {
			log.Println("/view: All ICE candidates for viewer have been sent.")
		}
	})

	// Log ICE connection state changes
	viewPeerConnection.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("/view: ICE Connection State has changed: %s\n", state.String())
	})

	// Parse the SDP from the request
	offer := webrtc.SessionDescription{}
	err = parseSDP(r, &offer)
	if err != nil {
		log.Println("/view: Invalid SDP received:", err)
		http.Error(w, "Invalid SDP", http.StatusBadRequest)
		return
	}
	log.Println("/view: SDP parsed successfully. SDP Type:", offer.Type.String())

	// Set the remote description
	err = viewPeerConnection.SetRemoteDescription(offer)
	if err != nil {
		log.Println("/view: Error setting remote description:", err)
		http.Error(w, "Could not set remote description", http.StatusInternalServerError)
		return
	}
	log.Println("/view: Remote description set.")

	// Create an answer and send it back
	answer, err := viewPeerConnection.CreateAnswer(nil)
	if err != nil {
		log.Println("/view: Error creating answer:", err)
		http.Error(w, "Could not create answer", http.StatusInternalServerError)
		return
	}

	err = viewPeerConnection.SetLocalDescription(answer)
	if err != nil {
		log.Println("/view: Error setting local description:", err)
		http.Error(w, "Could not set local description", http.StatusInternalServerError)
		return
	}
	log.Println("/view: Local description set. Sending SDP answer.")

	// Log the SDP for debugging purposes
	log.Printf("/view: Sending SDP answer to viewer: %s\n", answer.SDP)
	w.Write([]byte(answer.SDP))
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

	// Serve static JavaScript files
	http.Handle("/static/", http.FileServer(http.FS(content)))

	// Start the HTTP server
	log.Println("Server running at http://localhost:8080")
	err := http.ListenAndServe(":8080", nil)
	if err != nil {
		log.Fatal("Server failed:", err)
	}
}
