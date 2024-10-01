let peerConnection;
const servers = {
    iceServers: [
        {
            urls: "stun:stun.l.google.com:19302", // Google's public STUN server
        },
    ],
};

// Add event listeners when the DOM content is fully loaded
document.addEventListener("DOMContentLoaded", () => {
    document.getElementById("startPublisherButton").addEventListener("click", startPublisher);
    document.getElementById("startViewerButton").addEventListener("click", startViewer);
    checkMediaDevices(); // Check for available media devices
});

// Function to check available media devices (camera, microphone)
function checkMediaDevices() {
    console.log("Checking available media devices...");
    navigator.mediaDevices.enumerateDevices()
        .then(devices => {
            if (devices.length === 0) {
                console.error("No media devices found.");
            }
            devices.forEach(device => {
                console.log(`${device.kind}: ${device.label} (id: ${device.deviceId})`);
            });
        })
        .catch(err => {
            console.error("Error enumerating devices:", err);
        });
}

// Function to start publishing (uploading) the video stream
async function startPublisher() {
    try {
        console.log("Requesting access to media devices...");

        const constraints = { video: true, audio: false };  // Define media constraints for video and audio
        const stream = await navigator.mediaDevices.getUserMedia(constraints); // Request media stream
        console.log("Publisher's media stream acquired:", stream);

        // Add the stream to a video element to show local preview
        document.body.appendChild(createVideoElement(stream));

        // Create a new RTCPeerConnection
        peerConnection = new RTCPeerConnection({
            iceServers: [{
              urls: 'stun:stun.l.google.com:19302'
            }]
        });

        // Add the media stream's tracks to the peer connection
        stream.getTracks().forEach((track) => {
            //console.log(`Track being added to peer connection - Kind: ${track.kind}, Label: ${track.label}`);
            peerConnection.addTrack(track, stream);  // Add track to peer connection
        });

        // Log all senders
        //logSenders();

        // Handle ICE candidates
        peerConnection.onicecandidate = event => {
            if (event.candidate) {
                console.log("Sending ICE candidate to the server.");
                fetch('/ice-candidate-p', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(event.candidate)
                }).then(() => {
                    console.log("ICE candidate sent successfully.");
                }).catch(error => {
                    console.error("Error sending ICE candidate:", error);
                });
            }
        };

        try {
        
            const offer = await peerConnection.createOffer();
            await peerConnection.setLocalDescription(offer);
            console.log("Offer created and set as local description.");
    
            const response = await fetch('/publish', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify(offer)
            });
            const answer = await response.json();

            console.log("Received answer from the server.", answer, response);

    
            await peerConnection.setRemoteDescription(answer);
            console.log("Answer set as remote description.");
        } catch (error) {
            console.error("Error during offer/answer exchange:", error);
        }



        // Handle incoming ICE candidates from the server
        const handleIncomingICECandidate = async (candidate) => {
            try {
                await peerConnection.addIceCandidate(candidate);
                console.log("Added received ICE candidate.");
            } catch (e) {
                console.error('Error adding received ice candidate', e);
            }
        };

        // Poll the server for ICE candidates
        setInterval(async () => {
            try {
                const response = await fetch('/ice-candidates-p');
                const candidates = await response.json();
                if (candidates) {
                    candidates.forEach(handleIncomingICECandidate);
                    console.log("Polled and added ICE candidates from the server.");
                }
            } catch (error) {
                console.error("Error polling ICE candidates:", error);
            }
        }, 1000);

        // Handle ICE connection state changes
        peerConnection.oniceconnectionstatechange = function() {
            console.log("ICE connection state:", peerConnection.iceConnectionState);
        };

        // Handle connection state changes
        peerConnection.onconnectionstatechange = (event) => {
            console.log("Publisher connection state:", peerConnection.connectionState);
        };


    } catch (error) {
        console.error("Error getting media stream:", error);
        alert(`Error getting media stream: ${error.name} - ${error.message}`);
    }
}



// Function to start viewing (downloading) the video stream
async function startViewer() {
    try {
        console.log("Starting viewer...");
        
        const constraints = { video: true, audio: false };  // Define media constraints for video and audio
        const stream = await navigator.mediaDevices.getUserMedia(constraints); // Request media stream
        console.log("media stream acquired:", stream);


        // Create a new RTCPeerConnection
        peerConnection = new RTCPeerConnection({
            iceServers: [{
              urls: 'stun:stun.l.google.com:19302'
            }]
        });

        // Add the media stream's tracks to the peer connection
        stream.getTracks().forEach((track) => {
            peerConnection.addTrack(track, stream);  // Add track to peer connection
        });



        // Handle incoming tracks from the publisher
        peerConnection.ontrack = (event) => {
            console.log("Received track from publisher:", event.track);
            const [remoteStream] = event.streams;
            document.body.appendChild(createVideoElement(remoteStream)); // Show remote video
            console.log("Viewer displaying remote stream:", remoteStream);
        };

        // Handle ICE candidates
        peerConnection.onicecandidate = event => {
            if (event.candidate) {
                console.log("Sending ICE candidate to the server.");
                fetch('/ice-candidate-v', {
                    method: 'POST',
                    headers: { 'Content-Type': 'application/json' },
                    body: JSON.stringify(event.candidate)
                }).then(() => {
                    console.log("ICE candidate sent successfully.");
                }).catch(error => {
                    console.error("Error sending ICE candidate:", error);
                });
            }
        };

        // Handle ICE connection state changes
        peerConnection.oniceconnectionstatechange = function() {
            console.log("ICE connection state:", peerConnection.iceConnectionState);
        };

        // Handle connection state changes
        peerConnection.onconnectionstatechange = (event) => {
            console.log("Viewer connection state:", peerConnection.connectionState);
        };

        try {
        
            const offer = await peerConnection.createOffer();
            await peerConnection.setLocalDescription(offer);
            console.log("Offer created and set as local description.");
    
            const response = await fetch('/view', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify(offer)
            });
            const answer = await response.json();
            console.log("Received answer from the server.", response);
            console.log("Received answer from the server.", answer);
    
            await peerConnection.setRemoteDescription(answer);
            console.log("Answer set as remote description.");
        } catch (error) {
            console.error("Error during offer/answer exchange:", error);
        }

        // Handle incoming ICE candidates from the server
        const handleIncomingICECandidate = async (candidate) => {
            try {
                await peerConnection.addIceCandidate(candidate);
                console.log("Added received ICE candidate.");
            } catch (e) {
                console.error('Error adding received ice candidate', e);
            }
        };

        // Poll the server for ICE candidates
        setInterval(async () => {
            try {
                const response = await fetch('/ice-candidates-v');
                const candidates = await response.json();
                if (candidates) {
                    candidates.forEach(handleIncomingICECandidate);
                    console.log("Polled and added ICE candidates from the server.");
                }
            } catch (error) {
                console.error("Error polling ICE candidates:", error);
            }
        }, 1000);

    } catch (error) {
        console.error("Error starting viewer:", error);
        alert(`Error starting viewer: ${error.name} - ${error.message}`);
    }
}

// Function to log the senders and their associated tracks
function logSenders() {
    console.log("Logging senders...");
    const senders = peerConnection.getSenders();
    if (senders.length === 0) {
        console.log("No senders attached to the peer connection.");
    } else {
        senders.forEach((sender, index) => {
            const track = sender.track;
            if (track) {
                console.log(`Sender ${index + 1}: Kind - ${track.kind}, Label - ${track.label}, ReadyState - ${track.readyState}`);
            } else {
                console.log(`Sender ${index + 1}: No track attached to this sender.`);
            }
        });
    }
}

// Utility function to create a video element and attach a stream
function createVideoElement(stream) {
    const video = document.createElement("video");
    video.srcObject = stream;
    video.autoplay = true;
    video.muted = true; // Mute local video element to avoid echo during publishing
    video.style = "width: 50%; margin: 10px; border: 2px solid black;";
    return video;
}
