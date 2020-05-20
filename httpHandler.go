package main

import (
	"encoding/binary"
	"fmt"
	"net"
	"net/http"
	"net/url"
)

func (torrent *Torrent) connectToTrackerHTTP() ([]PeerAddress, error) {
	response, err := http.Get(torrent.torrentData.buildHTTPTrackerRequest(torrent.client.peerID, 6886))
	if response != nil && response.StatusCode != 200 { // if the request was "successful" but had non-200 status code, set err so the err != nil check catches it
		err = fmt.Errorf("failed http request, status code %d", response.StatusCode)
	}
	if err != nil {
		return nil, err
	}

	peerAddresses, err := torrent.trackerConnection.parseHTTPTrackerResponse(response) // ...and parse it
	if err != nil {
		return nil, err
	}

	return peerAddresses, nil
}

func (data *TorrentData) buildHTTPTrackerRequest(peerID [20]byte, port uint16) string {
	trackerURL := safeURLParse(data.announceURL)

	trackerQueryArgs := url.Values{
		"compact":    []string{"1"},
		"downloaded": []string{"0"},
		"info_hash":  []string{string(data.infoDict.sha1Hash[:])},
		"left":       []string{fmt.Sprint(data.infoDict.totalLength)},
		"peer_id":    []string{string(peerID[:])}, // have to use [:] to trick Go into converting our fixed-size array into a string (usually it converts slices)
		"port":       []string{fmt.Sprint(port)},
		"uploaded":   []string{"0"},
	}
	trackerURL.RawQuery = trackerQueryArgs.Encode()

	return trackerURL.String()
}

func (tracker *TrackerConnection) parseHTTPTrackerResponse(response *http.Response) ([]PeerAddress, error) {
	trackerResponse := safeUnmarshal(safeReadHTTPResponse(response)).(map[string]interface{})

	// Check for a failure reason; if there is one, panic and pass it up to the client
	if failureReason := trackerResponse["failure reason"]; failureReason != nil {
		return nil, fmt.Errorf("tracker rejected the connection: %s", failureReason)
	}

	// If there was no failure, then the following basic fields can be filled
	tracker.interval = uint32(trackerResponse["interval"].(int64)) // mandatory, or we won't know how often to poll
	if minInterval := trackerResponse["min interval"]; minInterval != nil {
		tracker.minInterval = uint32(minInterval.(int64))
	}
	if trackerID := trackerResponse["tracker id"]; trackerID != nil {
		tracker.trackerID = string(trackerID.([]uint8))
	}
	if nPeers := trackerResponse["complete"]; nPeers != nil {
		tracker.complete = uint32(nPeers.(int64))
	}
	if nLeechers := trackerResponse["incomplete"]; nLeechers != nil {
		tracker.incomplete = uint32(nLeechers.(int64))
	}

	return getPeerAddresses(trackerResponse["peers"]) // getting the peers is more complicated, so do it in a seperate function
}

// Parses the tracker's list of peers or bytestring of peer data into a list of PeerAddress structs, to be connected to later
func getPeerAddresses(peersResponse interface{}) ([]PeerAddress, error) {
	switch t := peersResponse.(type) {
	default:
		return nil, fmt.Errorf("bad type: %t", t)
	case []interface{}: // if peers were returned as a list of dicts, parsing them is trivial
		peerList := peersResponse.([]interface{})
		finalPeersList := make([]PeerAddress, len(peerList))

		for a, peerInfo := range peerList {
			peerDict := peerInfo.(map[string]interface{})
			peerIP := net.ParseIP(string(peerDict["ip"].([]uint8))) // if the IP is returned as an IPv6 hexed string or a IPv4 dotted quad string, we can parse and be done with it
			if peerIP == nil {                                      // if this parsing failed, it means the 'IP' was given as a DNS-resolvable URL string...
				peerLookupIP, err := net.LookupIP(string(peerDict["ip"].([]uint8))) // ... so try to resolve it, storing the first returned IP if successful...
				if err != nil {                                                     // ..and if it doesn't resolve, then don't store this useless peer
					continue
				} else {
					peerIP = peerLookupIP[0]
				}
			}
			finalPeersList[a].IP = peerIP // if the DNS lookup did resolve, or the IP was a normal format, store the new peer's data
			finalPeersList[a].port = peerDict["port"].(uint16)
		}

		return finalPeersList, nil
	case []uint8: // if peers were returned as a bytestring, parsing them is slightly more annoying
		peerBytestring := peersResponse.([]uint8)
		peerCount := len(peerBytestring) / 6 // peers in a bytestring are encoded in 6 consecutive bytes; 4 for an IPv4 address, 2 for a port
		finalPeersList := make([]PeerAddress, peerCount)

		for a := 0; a < peerCount; a++ {
			peerStartIndex := a * 6
			finalPeersList[a].IP = net.IP(peerBytestring[peerStartIndex:(peerStartIndex + 4)])
			finalPeersList[a].port = binary.BigEndian.Uint16(peerBytestring[(peerStartIndex + 4):(peerStartIndex + 6)])
		}

		return finalPeersList, nil
	}
}
