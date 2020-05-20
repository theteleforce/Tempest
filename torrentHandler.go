package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Constants //

// BitTorrentHashIdentifier is the prefix every info dict hash has in a magnet link (if it isn't there, the magnet link won't work with BitTorrent)
const BitTorrentHashIdentifier = "urn:btih:"

// ClientInfoFilepath is the path to the file holding client information
const ClientInfoFilepath = "info"

// InfoDictPieceSize is the size of each piece of the infoDict when we're getting it from peers
const InfoDictPieceSize = 16384 // 16 KiB, fixed by protocol

// MaxTrackerRetries is the number of times the client will attempt to connect to a tracker before panicking
const MaxTrackerRetries = 5

// PeerRequestTimeout is the number of seconds we'll wait for peers from DHT before we give up
const PeerRequestTimeout = float64(20)

// SavedTorrentFilesPath is the path to .torrent files that we converted from magnet links
const SavedTorrentFilesPath = "convertedTorrents"

// SeedingPort is the port that we tell our trackers we seed on
const SeedingPort = 6886

// RootTempDir is the directory that partial torrent files are stored in until their torrent is complete
const RootTempDir = "tmp"

// TimeBetweenTrackerRetries is the (constant) time between tracker retries (exponential backoff is not used to avoid putting people in limbo for 10 minutes as to whether their download will start)
const TimeBetweenTrackerRetries = 10 * time.Second

// TorrentDataChunkSize is the size (in bytes) at which we store the most recently-downloaded torrent data to disk
const TorrentDataChunkSize = uint64(2000000000)

// Structs //

// UndownloadedPieceInfo stores enough information about a piece in a torrent to download it
type UndownloadedPieceInfo struct {
	length   uint32
	index    uint32
	sha1Hash []byte
}

// InfoDictPiece is a piece of the info dict for a torrent, retrieved from a peer
type InfoDictPiece struct {
	length uint64
	data   []byte
	index  uint64
}

// PartialPieceData stores the data for a piece while it's downloading
type PartialPieceData struct {
	bytesDownloaded    uint32
	bytesRequested     uint32
	content            []byte
	numPendingRequests uint8
}

// PieceData stores the bytes extracted from a piece, along with the index
type PieceData struct {
	index   uint32
	content []byte
}

// TrackerConnection stores all the information from the torrent's tracker (list of peers, interval, etc.)
type TrackerConnection struct {
	complete    uint32
	incomplete  uint32
	interval    uint32
	minInterval uint32
	numPeers    uint8
	peers       []*Peer
	trackerID   string
}

// TorrentClient holds all the ongoing torrents, as well as channels for communicating with other subroutines
type TorrentClient struct {
	peerID          [20]byte
	ipcChannels     *TorrentIPCChannels
	ongoingTorrents map[string]*Torrent
}

// TorrentData holds metadata from a torrent file
type TorrentData struct {
	announceURL  string
	announceList []interface{} // only used for consistency when saving torrent files; this type storage is most convenient for the bencoding library
	creationDate int64
	comment      string
	createdBy    string
	encoding     string
	infoDict     TorrentInfoDict
}

// TorrentFile contains info about a single file in a torrent
type TorrentFile struct {
	name   string
	length uint64
	md5sum string
}

// Torrent is the main struct that starts and completes a torrent download, and contains all necessary information to do so
type Torrent struct {
	client            *TorrentClient
	tempDir           string
	trackerConnection TrackerConnection
	torrentData       TorrentData
}

// TorrentInfoDict holds data about the actual file(s) we'll be downloading
type TorrentInfoDict struct {
	files       []TorrentFile
	totalLength uint64
	md5sum      string
	name        string
	pieceLength uint32
	private     int8
	pieceHashes [][]byte
	sha1Hash    [20]byte
}

// UnpackingPiecesInfo stores the data we need when we're turning our saved piece chunks into a finished file
type UnpackingPiecesInfo struct {
	chunkFileNames         []string
	currentPieceDict       map[uint32][]byte
	currentPieceDictIndex  uint32
	currentPieceBytesIndex uint32
	totalPieces            uint32
}

// TORRENT CLIENT //

func createTorrentClient(channels *TorrentIPCChannels) *TorrentClient {
	var torrentClient TorrentClient

	torrentClient.getPeerID()
	torrentClient.ipcChannels = channels
	torrentClient.ongoingTorrents = make(map[string]*Torrent)
	return &torrentClient
}

func (torrentClient *TorrentClient) getPeerID() {
	// Read stored client data, load peer ID if we have it
	fileContents, err := ioutil.ReadFile(ClientInfoFilepath)
	if err != nil || len(fileContents) < 20 { // if we can't read the file (doesn't exist, corruption, permissions, etc) or it's corrupted...
		err = os.Remove(ClientInfoFilepath)
		if err != nil && !os.IsNotExist(err) { // if there was an error deleting it and it's still there...
			panic("Error accessing the info file! (if it's open in other programs, you should probably close it)") // ...we've got a problem
		}
		infoFile := safeCreateFile(ClientInfoFilepath) // ... create a new info file and write it
		defer infoFile.Close()

		rand.Read(torrentClient.peerID[:])           // gen peer ID as 20 random bytes...
		safeWrite(infoFile, torrentClient.peerID[:]) // ...and save it to the file
	} else {
		copy(torrentClient.peerID[:], fileContents) // the first 20 bytes of the file are the peer ID
	}
}

func (torrentClient *TorrentClient) dropTorrent(infoHash []byte) {
	encodedHash := hex.EncodeToString(infoHash)
	if torrentClient.ongoingTorrents[encodedHash] != nil {
		delete(torrentClient.ongoingTorrents, encodedHash)
		// also send a message to the gui?
	}
}

func (torrentClient *TorrentClient) start() {
	for true {
		select {
		default:
			select {
			default:
				time.Sleep(SubprocessSleepTime)
			case dhtRequest := <-torrentClient.ipcChannels.incomingDhtRequestChannel:
				torrentClient.handleDhtRequest(dhtRequest)
			}
		case guiRequest := <-torrentClient.ipcChannels.incomingGuiRequestChannel:
			torrentClient.handleGuiRequest(guiRequest)
		}
	}
}

func (torrentClient *TorrentClient) handleGuiRequest(guiRequest *IPCRequest) { // placeholder
	switch guiRequest.functionCall {
	default:
		torrentClient.ipcChannels.outgoingGuiReplyChannel <- &IPCReply{guiRequest.messageID, nil, fmt.Errorf("no function call with that name exists")}
	case "downloadFromFile":
		println("torrent client got request downloadFromFile")
		switch guiRequest.args[0].(type) {
		default:
			torrentClient.ipcChannels.outgoingGuiReplyChannel <- &IPCReply{guiRequest.messageID, nil, fmt.Errorf("couldn't parse filename as string")}
		case string:
			torrentClient.ipcChannels.outgoingGuiReplyChannel <- &IPCReply{guiRequest.messageID, nil, torrentClient.downloadFromFile(guiRequest.args[0].(string))}
		}
	case "downloadFromMagnetLink":
		println("torrent client got request downloadFromMagnetLink")
		switch guiRequest.args[0].(type) {
		default:
			torrentClient.ipcChannels.outgoingGuiReplyChannel <- &IPCReply{guiRequest.messageID, nil, fmt.Errorf("couldn't parse magnet link as string")}
		case string:
			torrentClient.ipcChannels.outgoingGuiReplyChannel <- &IPCReply{guiRequest.messageID, nil, torrentClient.downloadFromMagnetLink(guiRequest.args[0].(string))}
		}
	}
}

func (torrentClient *TorrentClient) handleDhtRequest(dhtRequest *IPCRequest) { // placeholder
	println(dhtRequest)
}

func (torrentClient *TorrentClient) downloadFromFile(filename string) error {
	newTorrent, err := parseNewTorrentFile(filename)
	if err != nil {
		return fmt.Errorf("couldn't parse torrent file: %s", err)
	}

	return torrentClient.addTorrentAndStart(newTorrent)
}

func (torrentClient *TorrentClient) downloadFromMagnetLink(link string) error {
	newTorrent, err := parseNewMagnetLink(link)
	if err != nil {
		return fmt.Errorf("couldn't parse magnet link: %s", err)
	}

	return torrentClient.addTorrentAndStart(newTorrent)
}

func (torrentClient *TorrentClient) addTorrentAndStart(newTorrent *Torrent) error {
	for encodedInfoHash := range torrentClient.ongoingTorrents {
		if hex.EncodeToString(newTorrent.torrentData.infoDict.sha1Hash[:]) == encodedInfoHash {
			return fmt.Errorf("failed to add torrent file: already downloading a torrent with that hash")
		}
	}

	newTorrent.makeTempDirectory()
	// if we've gotten here, it's safe to start downloading this torrent
	torrentClient.ongoingTorrents[hex.EncodeToString(newTorrent.torrentData.infoDict.sha1Hash[:])] = newTorrent // add it to our list of ongoing torrents
	newTorrent.client = torrentClient
	go newTorrent.download()
	return nil // no error means download started successfully
}

func (torrent *Torrent) makeTempDirectory() {
	torrent.tempDir = fmt.Sprintf("%s/%s", RootTempDir, hex.EncodeToString(torrent.torrentData.infoDict.sha1Hash[:]))
	if _, err := os.Stat(torrent.tempDir); os.IsNotExist(err) {
		err = os.MkdirAll(torrent.tempDir, os.ModePerm)
		if err != nil {
			panic(fmt.Sprintf("Couldn't create temp directory for torrent at %s (might be missing permissions)", torrent.tempDir))
		}
	}
}

func (torrent *Torrent) download() {
	maxPeers, neededInfoDict := torrent.getInitialPeers()

	//allFilesData := torrent.downloadLoop(maxPeers, neededInfoDict)
	chunkFileNames, err := torrent.downloadLoop(maxPeers, neededInfoDict)
	if err != nil {
		fmt.Printf("Failed to download torrent with info hash %s: %s\n") // TEMPORARY; should also write to gui channel to tell it that the torrent failed
	} else {
		//torrent.saveFiles(allFilesData)
		torrent.reassembleChunks(chunkFileNames)
		println("successful download!")
	}

}

func (torrent *Torrent) downloadLoop(maxPeers int, neededInfoDict bool) /*[]byte*/ ([]string, error) {
	//allFilesData := make([]byte, torrent.torrentData.infoDict.totalLength)

	// start by putting all the pieces in the channel work channel
	piecesRemainingChannel := make(chan *UndownloadedPieceInfo, len(torrent.torrentData.infoDict.pieceHashes))
	piecesFinishedChannel := make(chan *PieceData, len(torrent.torrentData.infoDict.pieceHashes))
	for pieceIndex, pieceHash := range torrent.torrentData.infoDict.pieceHashes {
		piecesRemainingChannel <- &UndownloadedPieceInfo{torrent.getPieceSize(pieceIndex), uint32(pieceIndex), pieceHash}
	}
	fmt.Println("Total pieces:", len(torrent.torrentData.infoDict.pieceHashes))

	// then start each initial peer concurrently downloading in the background
	for a := 0; uint8(a) < torrent.trackerConnection.numPeers; a++ {
		go torrent.trackerConnection.peers[a].startDownload(piecesRemainingChannel, piecesFinishedChannel, neededInfoDict, torrent.torrentData.infoDict.sha1Hash[:])
	}

	var err error // have to define this here so reassigning values in different contexts works well
	bytesDownloadedSinceStoring := uint64(0)
	chunksStored := 0
	chunkFileNames := make([]string, (torrent.torrentData.infoDict.totalLength/TorrentDataChunkSize)+1) // enough filenames for all the stored dicts assuming each dict is stored with exactly as many bytes as it can hold
	lastPeerRequestTime := time.Now()
	piecesDownloaded := 0
	piecesMap := make(map[uint32][]byte)
	for true {
		select {
		default:
			if (torrent.trackerConnection.numPeers != uint8(maxPeers)) && (time.Since(lastPeerRequestTime).Seconds() > torrent.peerRequestWaitTime(maxPeers)) { // if it's been long enough since we got new peers, try to get more
				fmt.Printf("getting new peers for torrent with hash %s\n", torrent.torrentData.infoDict.sha1Hash[:])
				newPeerStartIndex, newPeerCount := torrent.getNewPeers(maxPeers)
				for a := newPeerStartIndex; a < newPeerStartIndex+newPeerCount; a++ {
					go torrent.trackerConnection.peers[a].startDownload(piecesRemainingChannel, piecesFinishedChannel, false, torrent.torrentData.infoDict.sha1Hash[:]) // if we've already started, we can safely set needInfoDict to false, since there's no way we'd have to reconnect to these peers
				}
			} else { // otherwise, wait a bit and check again
				time.Sleep(SubprocessSleepTime)
			}
		case downloadedPiece := <-piecesFinishedChannel: // if there's a new piece, put it in the data buffer
			fmt.Printf("Got piece %d/%d\n", downloadedPiece.index, len(torrent.torrentData.infoDict.pieceHashes))
			piecesMap[downloadedPiece.index] = downloadedPiece.content
			bytesDownloadedSinceStoring += uint64(len(downloadedPiece.content)) // cast is safe because pieces don't have anywhere close to 2 gb (the size around which int32s would behave badly)

			if bytesDownloadedSinceStoring > TorrentDataChunkSize { // if we've stored enough data to save to disk...
				newFilename, err := torrent.storePieceMapToDisk(piecesMap, piecesDownloaded) // ...write it to disk and store the filename to reassemble from later
				if err != nil {
					return nil, fmt.Errorf("failed to store piece dict to disk: %s", err)
				}
				chunkFileNames = append(chunkFileNames, newFilename)
				chunksStored++
			}
			piecesDownloaded++

			/*copy(allFilesData[(downloadedPiece.index*torrent.torrentData.infoDict.pieceLength):], downloadedPiece.content)
			piecesDownloaded++*/
			if piecesDownloaded == len(torrent.torrentData.infoDict.pieceHashes) { // if this was the last piece, exit the loop so we can save the file
				chunkFileNames[chunksStored], err = torrent.storePieceMapToDisk(piecesMap, piecesDownloaded) // store the last chunk
				if err != nil {
					return nil, fmt.Errorf("failed to store last piece map to disk in torrent with hash %s: %s", torrent.torrentData.infoDict.sha1Hash[:], err)
				}
				println("downloaded last piece")
				//return allFilesData
				return chunkFileNames, nil
			}
		}
	}

	panic("Something is irreconcilably doomed in the download loop (just restart Tempest)") // excuse me?
}

func (torrent *Torrent) storePieceMapToDisk(piecesMap map[uint32][]byte, piecesDownloaded int) (string, error) {
	pieceFilename := fmt.Sprintf("%s/%d", torrent.tempDir, piecesDownloaded)
	pieceData := encodePiecesDict(piecesMap)
	fp, err := os.Create(pieceFilename)
	if err != nil {
		return "", fmt.Errorf("couldn't store piece map on disk: %s", err)
	}
	fp.Write(pieceData)        // ...save the bencoded dict of the pieces in memory to disk...
	for k := range piecesMap { // ...and delete those pieces from memory to make room for others
		delete(piecesMap, k)
	}

	return pieceFilename, nil
}

func (torrent *Torrent) reassembleChunks(chunkFileNames []string) error {
	currentPieceInfo, err := createPieceInfo(chunkFileNames, len(torrent.torrentData.infoDict.pieceHashes))
	if err != nil {
		return err
	}

	for _, file := range torrent.torrentData.infoDict.files {
		err = fillFileFromPieces(&file, currentPieceInfo)
		if err != nil {
			if err.Error() == "done" {
				os.RemoveAll(torrent.tempDir) // delete the temp dir if we're done with it
				return nil
			}
			os.RemoveAll(torrent.tempDir) // delete the temp dir if we're done with it
			return err
		}

		/*currentFileDataIndex := uint64(0)
		fp := safeCreateFile(file.name)
		defer fp.Close() // remember to close this file if there's an error writing to it

		pieceData = pieceMap[nextPiece] // try to get the piece data from the chunk we currently have loaded
		if pieceData == nil {           // if it's not there...
			pieceMap, err = getMapWithPiece(chunkFileNames, nextPiece) // ...get the map that has it
			if err != nil {
				return err
			}
			pieceData = pieceMap[nextPiece]
			currentPieceDataIndex = uint64(0) // reset the piece data index if we get a new piece
		}

		if uint64(len(pieceData)) > file.length { // cast is safe because len(PieceData) is always 2,000,000,000 or less
			safeWrite(fp, pieceData[:file.length]) // if there's more data in the piece than left in the file, empty the piece into the file and get the next piece
		} else {
			safeWrite(fp, pieceData)
		}
		safeWrite(fileConnection, allFilesData[dataIndex:(dataIndex+file.length)]) // write the bytes corresponding to this file in the array to the file
		dataIndex += file.length
		fp.Close() // if it succeeded, we can go ahead and close the file now (calling Close() twice makes Go unhappy but doesn't break anything afaict)*/
	}

	os.RemoveAll(torrent.tempDir) // delete the temp dir if we're done with it
	return fmt.Errorf("finished iterating through all files before reaching the end of the last piece (torrent with info hash %s)", torrent.torrentData.infoDict.sha1Hash[:])
}

func createPieceInfo(chunkFileNames []string, numPieces int) (*UnpackingPiecesInfo, error) {
	var piecesInfo UnpackingPiecesInfo
	piecesInfo.currentPieceBytesIndex = uint32(0)
	piecesInfo.currentPieceDictIndex = uint32(0)
	piecesInfo.totalPieces = uint32(numPieces) // cast is safe because numPieces is a positive int32 by default
	piecesInfo.chunkFileNames = chunkFileNames
	err := piecesInfo.getPieceMap()
	if err != nil {
		return nil, fmt.Errorf("creating UnpackingPiecesInfo failed: %s", err)
	}
	return &piecesInfo, nil
}

func (pieceInfo *UnpackingPiecesInfo) getPieceMap() error {
	if pieceInfo.currentPieceDictIndex == pieceInfo.totalPieces { // if this is the last piece...
		return fmt.Errorf("done") // ... we obviously can't get the next one, so say so
	}

	pieceInfo.currentPieceBytesIndex = uint32(0)                                                                  // start at the beginning of the next piece
	if pieceInThisDict := pieceInfo.currentPieceDict[pieceInfo.currentPieceDictIndex+1]; pieceInThisDict != nil { // if the piece we're looking for is in the map we currently have loaded...
		return nil // ...we obviously don't need to look for the map that has it
	}

	for _, filename := range pieceInfo.chunkFileNames { // otherwise, we have to look through our other stored piece dicts to find this piece
		piecesDict, err := decodePiecesDict(filename)
		if err != nil {
			return err
		}
		if pieceData := piecesDict[pieceInfo.currentPieceDictIndex]; pieceData != nil {
			pieceInfo.currentPieceDict = piecesDict
			return nil
		}
	}

	return fmt.Errorf("no chunk file found with piece at index %d", pieceInfo.currentPieceDictIndex)
}

func (pieceInfo *UnpackingPiecesInfo) getNextPieceAndMap() error {
	result := pieceInfo.getPieceMap()
	if result == nil {
		pieceInfo.currentPieceDictIndex++
	}
	return result
}

func fillFileFromPieces(file *TorrentFile, piecesInfo *UnpackingPiecesInfo) error { // returns a (current piece dict, current piece dict index, current piece data index) triplet
	bytesLeftInFile := file.length

	fp := safeCreateFile(file.name)
	defer fp.Close() // remember to close this file if there's an error writing to it

	for bytesLeftInFile > uint64(0) {
		pieceData := piecesInfo.currentPieceDict[piecesInfo.currentPieceDictIndex]
		/*pieceData = pieceMap[piecesInfo.currentPieceDictIndex] // try to get the piece data from the chunk we currently have loaded
		if pieceData == nil {           // if it's not there...
			err = piecesInfo.getNextPieceAndMap() // ...get the map that has it
			if err != nil {
				return err
			}
			pieceData = pieceMap[nextPiece]
		}*/

		if uint64(uint32(len(pieceData))-piecesInfo.currentPieceBytesIndex) > bytesLeftInFile { // cast is safe because len(PieceData) is always 2,000,000,000 or less
			_, err := fp.Write(pieceData[piecesInfo.currentPieceBytesIndex:(piecesInfo.currentPieceBytesIndex + uint32(bytesLeftInFile))]) // if there's more data in the piece than left in the file, empty the piece into the file and get the next piece
			if err != nil {
				return err
			}

			piecesInfo.currentPieceBytesIndex += uint32(bytesLeftInFile) // cast is safe because if we're here, bytesLeftInFile is less than the (originally int32) len(pieceData)
			return nil                                                   // ...and return, since we're done with the file
		}
		safeWrite(fp, pieceData[piecesInfo.currentPieceBytesIndex:]) // otherwise, write all the data left in the piece...
		err := piecesInfo.getNextPieceAndMap()                       // ...and move on to the next piece
		if err != nil {
			return err // pass the possible 'done' error up to the unpacking manager
		}
	}

	return nil
}

/*func getMapWithPiece(chunkFileNames []string, pieceIndex uint32) (map[uint32][]byte, error) {
	for _, filename := range chunkFileNames {
		piecesDict := decodePiecesDict(filename)
		if pieceData := piecesDict[pieceIndex]; pieceData != nil {
			return piecesDict, nil
		}
	}

	return nil, fmt.Errorf("no chunk file found with piece at index %d", pieceIndex)
}*/

func (torrent *Torrent) getNewPeers(maxPeers int) (uint8, uint8) {
	numStartingPeers := torrent.trackerConnection.numPeers

	startTime := time.Now()
	for true {
		if time.Since(startTime).Seconds() > PeerRequestTimeout { // if we've spent too long trying to fetch new peers, just give up and stick with what we have now
			return numStartingPeers, 0
		}
		// get peer addresses from the tracker and the DHT
		peerAddresses, err := torrent.getPeerAddresses(maxPeers, false) // since we've already started downloading, we can safely set needInfoDict to false
		if err != nil {                                                 // if fetching the addresses failed...
			fmt.Printf("supplemental getPeerAddresses failed; waiting 5 seconds and trying again...")
			time.Sleep(5 * time.Second) // ...wait a bit and try again.
			continue
		}

		// remove peer addresses that we're already connected to
		for a, peerAddress := range peerAddresses {
			for b := 0; uint8(b) < torrent.trackerConnection.numPeers; b++ {
				if bytes.Compare(peerAddress.IP, torrent.trackerConnection.peers[b].peerAddress.IP) == 0 {
					fmt.Printf("got duplicate peer %s in supplemental getNewPeers; discarding it\n", peerAddress.IP)
					peerAddresses[a] = PeerAddress{nil, 0}
					break
				}
			}
		}

		// try to connect to the remaining peer addresses; record how many were successful
		successfulNewPeers := torrent.connectToPeers(peerAddresses, maxPeers, false) // since we've already started downloading, we can safely set needInfoDict to false
		if successfulNewPeers == 0 {                                                 // if we couldn't connect to any of the peers we were given...
			fmt.Printf("supplemental connectToPeers failed; waiting 5 seconds and trying again...")
			time.Sleep(5 * time.Second) // ...wait a bit and try again
			continue
		}

		// if we've gotten here, peers were added successfully
		return numStartingPeers, uint8(successfulNewPeers)
	}

	panic("Something is incredibly doomed in getNewPeers (just restart Tempest)")
}

func (torrent *Torrent) peerRequestWaitTime(maxPeers int) float64 {
	fractionOfMax := float64(torrent.trackerConnection.numPeers) / float64(maxPeers)
	if fractionOfMax > float64(0.9) {
		return float64(390)
	} else if fractionOfMax > float64(0.75) {
		return float64(240)
	} else if fractionOfMax > float64(0.5) {
		return float64(120)
	} else if fractionOfMax > float64(0.25) {
		return float64(30)
	}
	return float64(10)
}

func (torrent *Torrent) evaluate() (int, bool) { // tells us how many peers we should ask for, and whether we need to ask them for the info dict or not
	if torrent.torrentData.infoDict.pieceHashes == nil { // if we don't have any torrent metadata yet...
		return MaxPeers, true // ...ask for it, and request the maximum number of peers, so we don't accidentially assign maxPeers = 0 below
	} else if len(torrent.torrentData.infoDict.pieceHashes) < MaxPeers { // never have more peers than there are pieces to download
		return len(torrent.torrentData.infoDict.pieceHashes), false // cast is safe, since MaxPeers is a uint8 itself
	} else {
		return MaxPeers, false
	}
}

func (torrent *Torrent) getInitialPeers() (int, bool) {
	peersNeeded, needInfoDict := torrent.evaluate()
	torrent.trackerConnection.peers = make([]*Peer, peersNeeded)
	for true {
		// get peer addresses from the tracker and the DHT
		peerAddresses, err := torrent.getPeerAddresses(peersNeeded, needInfoDict)
		if err != nil { // if fetching the addresses failed...
			fmt.Printf("initial getPeerAddresses failed; waiting 5 seconds and trying again...")
			time.Sleep(5 * time.Second) // ...wait a bit and try again.
			continue
		}

		// try to connect to those peer addresses; record how many were successful
		successfulPeers := torrent.connectToPeers(peerAddresses, peersNeeded, needInfoDict)
		if successfulPeers == 0 { // if we couldn't connect to any of the peers we were given...
			fmt.Printf("initial connectToPeers failed; waiting 5 seconds and trying again...")
			time.Sleep(5 * time.Second) // ...wait a bit and try again
			continue
		}
		fmt.Println("successful initial peers:", successfulPeers)

		// if we need torrent metadata, fetch it from peers who said they have it
		if needInfoDict {
			err := torrent.getInfoDictFromPeers()
			if err != nil { // if we couldn't get metadata from this batch of peers...
				fmt.Printf("initial getInfoDictFromPeers failed; waiting 5 seconds and trying again...")
				time.Sleep(5 * time.Second) // ...wait a bit and try again
				continue
			}
		}

		// if we got here, everything went through successfully
		return peersNeeded, needInfoDict
	}

	panic("Something is hilariously doomed in getInitialPeers (just restart Tempest)")
}

func (torrent *Torrent) establishNewPeers(peerAddresses []PeerAddress, peersRequested int, needInfoDict bool) int { // turn PeerAddress structs into peers
	successfulPeers := torrent.connectToPeers(peerAddresses, peersRequested, needInfoDict) // also fetches torrent metadata from peers if we need it
	fmt.Println("successful peers:", successfulPeers)

	if needInfoDict { // if we need torrent metadata, fetch it from peers who said they have it before we start
		err := torrent.getInfoDictFromPeers()
		if err != nil {
			fmt.Printf("error getting info dict: %s\n", err)
			for _, peer := range torrent.trackerConnection.peers {
				if peer != nil {
					peer.tcpConnection.Close()
				}
			}
			return 0
		}
		fmt.Println("info dict fetching successful")
	}

	return successfulPeers
}

func (torrent *Torrent) getPeerAddresses(peersNeeded int, needInfoDict bool) ([]PeerAddress, error) {
	peerAddresses := make([]PeerAddress, 0, peersNeeded)

	if torrent.torrentData.announceURL != "" { // if we have a tracker, get peers from it first
		newPeerAddresses, err := torrent.getPeersFromTracker()
		if err != nil {
			fmt.Printf("failed to get peers from tracker for torrent with hash %s: %s\n", torrent.torrentData.infoDict.sha1Hash[:], err)
		} else {
			peerAddresses = append(peerAddresses, newPeerAddresses...)
		}
	}

	// whether we have a tracker or not, we want peers from the DHT as well
	newPeerAddressesPrototype, err := torrent.callDht("getPeersFor", []interface{}{torrent.torrentData.infoDict.sha1Hash[:]})
	if err != nil {
		fmt.Printf("no peers found in DHT for torrent with hash %s: %s\n", torrent.torrentData.infoDict.sha1Hash[:], err)
	} else {
		newPeerAddresses := newPeerAddressesPrototype[0].([]PeerAddress) // [0] because newPeerAddressesPrototype has peerAddresses as the first and only return value
		peerAddresses = append(peerAddresses, newPeerAddresses...)
	}

	if len(peerAddresses) == 0 {
		return nil, fmt.Errorf("couldn't download torrent with hash %s: no peers found in tracker or DHT", torrent.torrentData.infoDict.sha1Hash[:])
	}
	return peerAddresses, nil // if we got here, the peer addresses are fine
}

func (torrent *Torrent) callDht(functionCall string, args []interface{}) ([]interface{}, error) {
	request, messageID := torrent.createRequest(functionCall, args)
	torrent.client.ipcChannels.outgoingDhtRequestChannel <- request

	startTime := time.Now()
	for true {
		select {
		default:
			if time.Since(startTime).Seconds() > PeerRequestTimeout {
				return nil, fmt.Errorf("request to DHT timed out")
			}
			time.Sleep(SubprocessSleepTime)
		case dhtResponse := <-torrent.client.ipcChannels.incomingDhtReplyChannel:
			if bytes.Compare(request.messageID, messageID) == 0 {
				if dhtResponse.err != nil {
					return nil, fmt.Errorf("request to DHT failed: %s", dhtResponse.err)
				}
				return dhtResponse.returnValues, nil
			}
			// if it wasn't a response to our message, put it back on the reply stack
			torrent.client.ipcChannels.incomingDhtReplyChannel <- dhtResponse
		}
	}

	return nil, fmt.Errorf("something went unspecifically and horribly wrong in callDht")
}

func (torrent *Torrent) downloadFromPeers(neededInfoDict bool) {
	piecesRemainingChannel := make(chan *UndownloadedPieceInfo, len(torrent.torrentData.infoDict.pieceHashes))
	piecesFinishedChannel := make(chan *PieceData, len(torrent.torrentData.infoDict.pieceHashes))
	for pieceIndex, pieceHash := range torrent.torrentData.infoDict.pieceHashes { // start by putting all the pieces in the channel of pieces that need to be done
		piecesRemainingChannel <- &UndownloadedPieceInfo{torrent.getPieceSize(pieceIndex), uint32(pieceIndex), pieceHash}
	}

	for _, peer := range torrent.trackerConnection.peers { // then start each peer concurrently downloading in the background
		if peer != nil { // nil peers are standins for new peers that may be connected to later
			go peer.startDownload(piecesRemainingChannel, piecesFinishedChannel, neededInfoDict, torrent.torrentData.infoDict.sha1Hash[:])
		}
	}

	fmt.Println("Total pieces:", len(torrent.torrentData.infoDict.pieceHashes))
	allFilesData := make([]byte, torrent.torrentData.infoDict.totalLength)
	for downloadedPieces := 0; downloadedPieces < len(torrent.torrentData.infoDict.pieceHashes); downloadedPieces++ { // until we've downloaded all the pieces...
		downloadedPiece := <-piecesFinishedChannel // block until one of the peers finishes downloading a piece
		fmt.Printf("Got piece %d/%d\n", downloadedPiece.index, len(torrent.torrentData.infoDict.pieceHashes))
		copy(allFilesData[(downloadedPiece.index*torrent.torrentData.infoDict.pieceLength):], downloadedPiece.content) // copy that piece's content into the final buffer
	}
	close(piecesRemainingChannel)
	close(piecesFinishedChannel)

	torrent.saveFiles(allFilesData)
}

func (torrent *Torrent) sendDhtRequest(functionCall string, args []interface{}) []byte {
	request, ID := torrent.createRequest(functionCall, args)
	torrent.client.ipcChannels.outgoingDhtRequestChannel <- request
	return ID
}

func (torrent *Torrent) sendGuiRequest(functionCall string, args []interface{}) []byte {
	request, ID := torrent.createRequest(functionCall, args)
	torrent.client.ipcChannels.outgoingGuiRequestChannel <- request
	return ID
}

func (torrent *Torrent) createRequest(functionCall string, args []interface{}) (*IPCRequest, []byte) {
	var request IPCRequest
	request.genMessageID()
	request.functionCall = functionCall
	request.args = args

	return &request, request.messageID[:]
}

func (torrent *Torrent) downloadFromMagnetLink(link string) {
	queryParams, err := url.ParseQuery(link[8:]) // 7: skips over the functionally useless magnet: at the start of every magnet link
	if err != nil {
		panic(fmt.Sprintf("Invalid magnet link (couldn't decode query args): %s", link))
	}

	infoDictHashHex := queryParams["xt"]
	if infoDictHashHex == nil {
		panic(fmt.Sprintf("Invalid magnet link (no xt field): %s", link))
	}
	if string(infoDictHashHex[0][:9]) != BitTorrentHashIdentifier { // [0] because .ParseQuery returns a slice of results
		panic(fmt.Sprintf("Invalid magnet link (provided xt field is for a protocol other than BitTorrent):", link))
	}
	sha1Hash, err := hex.DecodeString(infoDictHashHex[0][9:]) // if valid, the info dict hash should be the tenth char onward of the xt field
	if err != nil {
		panic(fmt.Sprintf("Invalid magnet link (provided xt field has an invalid hash): %s", link))
	}
	copy(torrent.torrentData.infoDict.sha1Hash[:], sha1Hash) // copy is safe because only 20 bytes can be copied

	peerAddresses := make([]PeerAddress, MaxPeers)
	if trackerLinks := queryParams["tr"]; trackerLinks != nil { // if there's at least one tracker link, try to get peers from it and download from there
		for _, trackerLink := range trackerLinks { // try each tracker link in turn
			torrent.torrentData.announceURL = trackerLink
			peerAddresses, err = torrent.getPeersFromTracker()
			if err == nil { // if we successfully get peers, then move on to the next step
				break
			}
		}
	} else {
		println("whoops")
		// if there's no tracker link, try to look up the hash in the DHT
	}
	if peerAddresses[0].IP == nil {
		panic(fmt.Sprintf("Couldn't get peers for download of magnet link %s!", link))
	}

	torrent.startDownload(peerAddresses)
}

func parseNewMagnetLink(link string) (*Torrent, error) {
	var torrent Torrent
	queryParams, err := url.ParseQuery(link[8:]) // 7: skips over the functionally useless magnet: at the start of every magnet link
	if err != nil {
		return nil, fmt.Errorf("incorrectly formatted magnet link (couldn't decode query args): %s", link)
	}

	infoDictHashHex := queryParams["xt"]
	if infoDictHashHex == nil {
		return nil, fmt.Errorf("invalid magnet link (no xt field): %s", link)
	}
	if string(infoDictHashHex[0][:9]) != BitTorrentHashIdentifier { // [0] because .ParseQuery returns a slice of results
		return nil, fmt.Errorf("invalid magnet link (provided xt field is for a protocol other than BitTorrent): %s", link)
	}
	sha1Hash, err := hex.DecodeString(infoDictHashHex[0][9:]) // if valid, the info dict hash should be the tenth char onward of the xt field
	if err != nil {
		return nil, fmt.Errorf("Invalid magnet link (provided xt field has an invalid hash): %s", link)
	}
	copy(torrent.torrentData.infoDict.sha1Hash[:], sha1Hash) // copy is safe because only 20 bytes can be copied

	return &torrent, nil
}

func (torrent *Torrent) downloadFromTorrentFile(filepath string) {
	hadTrackerLink, err := torrent.torrentData.parseTorrentFile(filepath)
	if err != nil {
		panic(err) // [TEMPORARY MEASURE]
	}
	var peerAddresses []PeerAddress
	if hadTrackerLink {
		peerAddresses, err = torrent.getPeersFromTracker()
		if err != nil {
			panic(err) // [TEMPORARY MEASURE]
		}
	} else {
		panic("fuck")
	}
	torrent.startDownload(peerAddresses)
}

func (torrent *Torrent) startDownload(peerAddresses []PeerAddress) {
	var maxPeers int // turn PeerAddress structs into Peers, then start downloading from them
	needExtension := false
	if torrent.torrentData.infoDict.pieceHashes == nil { // if we don't have any torrent metadata yet...
		needExtension = true // ...remember to ask for it from peers
		maxPeers = MaxPeers  // ...and ask for the maximum amount of peers, so we don't accidentially assign maxPeers = 0 below
	} else if len(torrent.torrentData.infoDict.pieceHashes) < MaxPeers { // never have more peers than there are pieces to download
		maxPeers = len(torrent.torrentData.infoDict.pieceHashes)
	} else {
		maxPeers = MaxPeers
	}

	successfulPeers := torrent.connectToPeers(peerAddresses, maxPeers, needExtension) // also fetches torrent metadata from peers if we need it
	fmt.Println("successful peers:", successfulPeers)

	if needExtension { // if we need torrent metadata, fetch it from peers who said they have it before we start
		err := torrent.getInfoDictFromPeers()
		if err != nil {
			fmt.Printf("error getting info dict: %s\n", err)
			for _, peer := range torrent.trackerConnection.peers {
				if peer != nil {
					peer.tcpConnection.Close()
				}
			}
			return
		}
		fmt.Println("info dict fetching successful")
	}

	piecesRemainingChannel := make(chan *UndownloadedPieceInfo, len(torrent.torrentData.infoDict.pieceHashes))
	piecesFinishedChannel := make(chan *PieceData, len(torrent.torrentData.infoDict.pieceHashes))
	for pieceIndex, pieceHash := range torrent.torrentData.infoDict.pieceHashes { // start by putting all the pieces in the channel of pieces that need to be done
		piecesRemainingChannel <- &UndownloadedPieceInfo{torrent.getPieceSize(pieceIndex), uint32(pieceIndex), pieceHash}
	}

	for _, peer := range torrent.trackerConnection.peers { // then start each peer concurrently downloading in the background
		if peer != nil { // nil peers are standins for new peers that may be connected to later
			go peer.startDownload(piecesRemainingChannel, piecesFinishedChannel, needExtension, torrent.torrentData.infoDict.sha1Hash[:])
		}
	}

	fmt.Println("Total pieces:", len(torrent.torrentData.infoDict.pieceHashes))
	allFilesData := make([]byte, torrent.torrentData.infoDict.totalLength)
	for downloadedPieces := 0; downloadedPieces < len(torrent.torrentData.infoDict.pieceHashes); downloadedPieces++ { // until we've downloaded all the pieces...
		downloadedPiece := <-piecesFinishedChannel // block until one of the peers finishes downloading a piece
		fmt.Printf("Got piece %d/%d\n", downloadedPiece.index, len(torrent.torrentData.infoDict.pieceHashes))
		copy(allFilesData[(downloadedPiece.index*torrent.torrentData.infoDict.pieceLength):], downloadedPiece.content) // copy that piece's content into the final buffer
	}
	close(piecesRemainingChannel)
	close(piecesFinishedChannel)

	torrent.saveFiles(allFilesData)
}

func (torrent *Torrent) getPeersFromTracker() ([]PeerAddress, error) {
	attempts := 0
	var connectionError error = nil // define here so we can use in the panic message if we fail too many times
	var peerAddresses []PeerAddress // define here so we can populate it from multiple functions in the loop

	for attempts < MaxTrackerRetries {
		if torrent.torrentData.announceURL[:3] == "udp" {
			println("connecting to tracker over udp")
			peerAddresses, connectionError = torrent.connectToTrackerUDP()
		} else {
			println("connecting to tracker over http")
			peerAddresses, connectionError = torrent.connectToTrackerHTTP()
		}
		if connectionError != nil {
			attempts++
			fmt.Printf("error connecting to tracker at %s: %s\n", torrent.torrentData.announceURL, connectionError)
			fmt.Println("retrying connection to tracker, attempt ", attempts, "...")
			time.Sleep(TimeBetweenTrackerRetries)
			continue
		}

		// if we've gotten here, we successfully fetched peer addresses from the tracker
		return peerAddresses, nil
	}

	// if we're still here and over the maximum attempts, abort the download
	return nil, fmt.Errorf("failed to fetch peer addresses from the tracker: %s", connectionError)
}

func (torrent *Torrent) getInfoDictFromPeers() error {
	consensusMetadataSize, err := getConsensusMetadataSize(torrent.trackerConnection.peers)
	if err != nil {
		return fmt.Errorf("no consensus metadata size: %s", err)
	}

	piecesInInfodict := consensusMetadataSize / InfoDictPieceSize
	if consensusMetadataSize%InfoDictPieceSize != 0 {
		piecesInInfodict++
	}
	lastPieceSize := consensusMetadataSize - ((piecesInInfodict - 1) * InfoDictPieceSize)

	infoDictPiecesRemainingChannel := make(chan uint64, piecesInInfodict)
	infoDictPiecesChannel := make(chan *InfoDictPiece, piecesInInfodict)
	defer close(infoDictPiecesRemainingChannel)
	defer close(infoDictPiecesChannel)

	for a := uint64(0); a < piecesInInfodict; a++ { // start by putting all the piece indices in a channel for workers to read
		infoDictPiecesRemainingChannel <- a
	}

	numPeersFetchingInfo := uint64(0)
	for _, peer := range torrent.trackerConnection.peers { // then, get all the peers who support returning metadata to give us the metadata
		if (peer != nil) && (peer.supportsExtended) {
			go peer.fetchInfoDict(infoDictPiecesRemainingChannel, infoDictPiecesChannel, piecesInInfodict, lastPieceSize)
			numPeersFetchingInfo++
		}
	}

	infoDictData := make([]byte, consensusMetadataSize)
	numFailedPeers := uint64(0)
	piecesRecieved := uint64(0)
	for (piecesRecieved < piecesInInfodict) && (numFailedPeers < numPeersFetchingInfo) {
		newPiece := <-infoDictPiecesChannel
		if newPiece == nil {
			numFailedPeers++
		} else {
			copy(infoDictData[(newPiece.index*InfoDictPieceSize):((newPiece.index*InfoDictPieceSize)+newPiece.length)], newPiece.data)
			piecesRecieved++
		}
	}
	if (numFailedPeers == numPeersFetchingInfo) && (piecesRecieved < piecesInInfodict) {
		return fmt.Errorf("all peers failed before the entire info dict could be retrieved")
	}

	sha1Sum := sha1.Sum(infoDictData)
	print(sha1Sum[:])
	if sha1.Sum(infoDictData) != torrent.torrentData.infoDict.sha1Hash { // if the hash of the downloaded infoDict doesn't match the hash we were looking for...
		return fmt.Errorf("info dict provided by peers doesn't match infoDict hash in torrent file/magnet link") // should be more descriptive
	}
	test := safeUnmarshal(infoDictData).(map[string]interface{})
	print(test)
	hasPieces := torrent.torrentData.infoDict.parseTorrentInfoDict(safeUnmarshal(infoDictData).(map[string]interface{})) // safeUnmarshal is always safe here; since we know that the hashes match, the data will always decode into a dict (shattered nonwithstanding :^))
	if !hasPieces {
		return fmt.Errorf("info dict provided by peers doesn't have 'pieces' field")
	}
	torrent.saveTorrentFile()
	return nil
}

func (torrent *Torrent) saveFiles(allFilesData []byte) {
	var dataIndex uint64 = 0
	for _, file := range torrent.torrentData.infoDict.files {
		fileConnection := safeCreateFile(file.name)
		defer fileConnection.Close() // remember to close this file if there's an error writing to it

		safeWrite(fileConnection, allFilesData[dataIndex:(dataIndex+file.length)]) // write the bytes corresponding to this file in the array to the file
		dataIndex += file.length
		fileConnection.Close() // if it succeeded, we can go ahead and close the file now (calling Close() twice makes Go unhappy but doesn't break anything afaict)
	}
}

func (torrent *Torrent) connectToPeers(peerAddresses []PeerAddress, maxPeers int, needExtension bool) int {
	peerStatusChannel := make(chan *Peer, maxPeers)

	actualPeers := 0
	responses := 0
	successfulConnections := 0

	peersToConnectTo := 0
	if len(peerAddresses) < maxPeers {
		peersToConnectTo = len(peerAddresses)
	} else {
		peersToConnectTo = maxPeers
	}
	for a := 0; a < peersToConnectTo; a++ {
		if peerAddresses[a].IP != nil {
			go peerAddresses[a].connect(peerStatusChannel, torrent.torrentData.infoDict.sha1Hash[:], needExtension)
			actualPeers++
		}
	}

	for responses < actualPeers {
		newPeer := <-peerStatusChannel
		if newPeer != nil { // if the peer successfully connected,
			if int(torrent.trackerConnection.numPeers)+successfulConnections < maxPeers { // never add more peers than the torrent can hold
				torrent.trackerConnection.peers[int(torrent.trackerConnection.numPeers)+successfulConnections] = newPeer // put it on the list of working peers
				successfulConnections++
			}
		}
		responses++
	}

	torrent.trackerConnection.numPeers += uint8(successfulConnections)
	return successfulConnections
}

func (torrent *Torrent) getPieceSize(pieceIndex int) uint32 { // since this is never called with user-provided input, assume that pieceIndex will always be valid
	finalPieceIndex := len(torrent.torrentData.infoDict.pieceHashes) - 1 // the uint32 cast we do later is safe unless there are 0 piece hashes, in which case there are bigger problems
	if pieceIndex == finalPieceIndex {
		return uint32(torrent.torrentData.infoDict.totalLength - (uint64(torrent.torrentData.infoDict.pieceLength) * uint64(finalPieceIndex))) // the last piece is whatever's remaining after the first n-1 pieces
	}
	return torrent.torrentData.infoDict.pieceLength // should this be cast to uint32? NO
}

func (torrent *Torrent) saveTorrentFile() {
	filepath := SavedTorrentFilesPath + "/" + hex.EncodeToString(torrent.torrentData.infoDict.sha1Hash[:]) + ".torrent"
	_, err := ioutil.ReadFile(filepath) // check if the file is already there
	if !os.IsNotExist(err) {
		println("[ERROR] failed to save magnet link as torrent file (have we already done that?)")
		return
	}

	if _, err := os.Stat(SavedTorrentFilesPath); os.IsNotExist(err) {
		err = os.MkdirAll(SavedTorrentFilesPath, os.ModePerm)
		if err != nil {
			fmt.Printf("[ERROR] failed to save magnet link as torrent file (couldn't create directory for file at %s)\n", filepath)
			return
		}
	}
	// if we've gotten here; create the file, encode the infoDict, and write it
	fp, err := os.Create(filepath)
	if err != nil {
		fmt.Printf("[ERROR] failed to save magnet link as torrent file (couldn't create file at %s)\n", filepath)
		return
	}
	defer fp.Close()

	encodedTorrentData, err := torrent.torrentData.encode()
	if err != nil {
		fmt.Printf("[ERROR] failed to save magnet link as torrent file (couldn't encode torrent data)\n")
		return
	}
	_, err = fp.Write(encodedTorrentData)
	if err != nil {
		fmt.Printf("[ERROR] failed to save magnet link as torrent file (couldn't write to file at %s)\n", filepath)
		return
	}
}

func (data *TorrentData) encode() ([]byte, error) {
	torrentDict := makeOrderedDict()

	if data.announceURL != "" {
		torrentDict.addKeyValuePair("announce", data.announceURL)
	}
	if data.comment != "" {
		torrentDict.addKeyValuePair("comment", data.comment)
	}
	if data.createdBy != "" {
		torrentDict.addKeyValuePair("created by", data.createdBy)
	}
	if data.creationDate != 0 {
		torrentDict.addKeyValuePair("creation date", data.creationDate)
	}
	if data.encoding != "" {
		torrentDict.addKeyValuePair("encoding", data.encoding)
	}

	info := makeOrderedDict()

	if len(data.infoDict.files) > 0 {
		if len(data.infoDict.files) == 1 {
			info.addKeyValuePair("length", data.infoDict.files[0].length)
			if data.infoDict.md5sum != "" {
				info.addKeyValuePair("md5sum", data.infoDict.files[0].md5sum)
			}
			info.addKeyValuePair("name", data.infoDict.files[0].name)
		} else {
			filesList := make([]*OrderedDict, len(data.infoDict.files))
			for a, file := range data.infoDict.files {
				fileDict := makeOrderedDict()

				fileDict.addKeyValuePair("length", file.length)
				if file.md5sum != "" {
					fileDict.addKeyValuePair("md5sum", file.md5sum)
				}
				fileDict.addKeyValuePair("path", stringListToInterfaceList(filepath.SplitList(file.name)))
				filesList[a] = fileDict
			}
			info.addKeyValuePair("files", filesList)

			if data.infoDict.md5sum != "" { // have to check this, because some sterling intellects decide to include these in addition to a 'files' key (in direct violation of BEP 3)
				info.addKeyValuePair("md5sum", data.infoDict.md5sum)
			}
			if data.infoDict.name != "" {
				info.addKeyValuePair("name", data.infoDict.name)
			}
		}
	}

	if data.infoDict.pieceLength != 0 {
		info.addKeyValuePair("piece length", data.infoDict.pieceLength)
	}

	if len(data.infoDict.pieceHashes) > 0 {
		concatenatedPieceHashes := make([]byte, 20*len(data.infoDict.pieceHashes))
		for a, pieceHash := range data.infoDict.pieceHashes {
			copy(concatenatedPieceHashes[(20*a):(20*(a+1))], pieceHash)
		}

		info.addKeyValuePair("pieces", concatenatedPieceHashes)
	}

	if data.infoDict.private != -1 {
		info.addKeyValuePair("private", data.infoDict.private)
	}
	torrentDict.addKeyValuePair("info", info)

	if data.announceList != nil {
		torrentDict.addKeyValuePair("url-list", data.announceList) // this naming is inconsistent as fuck, but it shows up in a million old torrents so oh well
	}

	return bencodeOrderedDict(torrentDict)
}

func parseNewTorrentFile(filepath string) (*Torrent, error) {
	var torrent Torrent

	_, err := torrent.torrentData.parseTorrentFile(filepath)
	return &torrent, err
}

func (data *TorrentData) parseTorrentFile(filepath string) (bool, error) { // returns whether or not there was a tracker link or not
	torrentInfoBytes := safeReadFile(filepath)
	torrentInfo, success := safeUnmarshal(torrentInfoBytes).(map[string]interface{})
	if !success {
		return false, fmt.Errorf("couldn't read the torrent file at %s -- it may be corrupted", filepath)
	}

	data.infoDict.sha1Hash = getInfoHash(torrentInfoBytes) // have to use the raw file data instead of re-bencoding the info dict, because this bencoding library doesn't support ordered dictionaries and the dictionaries being out of order ruins the hash
	data.infoDict.parseTorrentInfoDict(torrentInfo["info"].(map[string]interface{}))

	// could make a helper function to do these sort of assignments, but it would have to use inefficient lookup of struct properties by name, so I'd rather not
	if creationDate := torrentInfo["creation date"]; creationDate != nil {
		data.creationDate = creationDate.(int64)
	} // could use encoding, success := ... here, but i don't see how it's more helpful than just seeing if the wanted value is nil
	if comment := torrentInfo["comment"]; comment != nil {
		data.comment = string(comment.([]uint8))
	}
	if createdBy := torrentInfo["created by"]; createdBy != nil {
		data.createdBy = string(createdBy.([]uint8))
	}
	if encoding := torrentInfo["encoding"]; encoding != nil {
		data.encoding = string(encoding.([]uint8))
	}
	if announceList := torrentInfo["url-list"]; announceList != nil { // specs say this should be 'announce-list', but that's not what i'm seeing in torrents
		data.announceList = announceList.([]interface{})
	}
	if announceURL := torrentInfo["announce"]; announceURL != nil {
		data.announceURL = string(torrentInfo["announce"].([]uint8))
		return true, nil
	}
	return false, nil
}

func (infoDict *TorrentInfoDict) parseTorrentInfoDict(info map[string]interface{}) bool { // returns True if we have piece hashes from the file, False if we need to get them from peers
	/*test := safeMarshal(info)
	print(test)
	infoDict.sha1Hash = sha1.Sum(safeMarshal(info)) // slightly inefficient to re-bencode the dict (eventual fix?)*/
	if private := info["private"]; private != nil { // we don't actually do anything with this, because deadlines, but we need to store it if we want to reencode this data at some point
		infoDict.private = int8(private.(int64)) // cast is safe because private is always either -1, 0, or 1
	} else {
		infoDict.private = -1 // since 0 could be interpreted as "not private", this tells our reencoder not to include this key
	}

	if pieceLength := info["piece length"]; pieceLength != nil {
		infoDict.pieceLength = uint32(info["piece length"].(int64)) // this cast is sketchy
	}

	// Check if the torrent is one file or several. If it's one, we simplify by putting it alone in a list
	if files := info["files"]; files != nil {
		fileList := info["files"].([]interface{})
		infoDict.files = make([]TorrentFile, len(fileList))
		for a := 0; a < len(fileList); a++ {
			fileData := fileList[a].(map[string]interface{})
			filePath := filePathListToString(fileData["path"].([]interface{})) // cast is safe, since it's sha-1 guaranteed to be a parsable list of byte slices
			if md5Sum := fileData["md5sum"]; md5Sum != nil {
				infoDict.files[a] = TorrentFile{filePath, uint64(fileData["length"].(int64)), string(md5Sum.([]byte))}
			} else {
				infoDict.files[a] = TorrentFile{filePath, uint64(fileData["length"].(int64)), ""}
			}
			// infoDict.files[a] = TorrentFile{filePath, uint64(fileData["length"].(int64))} // the 'name' of a file in a multi-file torrent is actually the path that the file is stored at
			infoDict.totalLength += uint64(fileData["length"].(int64))
		}
	} else {
		if name := info["name"]; name != nil {
			if md5Sum := info["md5sum"]; md5Sum != nil {
				infoDict.files = []TorrentFile{TorrentFile{string(info["name"].([]uint8)), uint64(info["length"].(int64)), string(md5Sum.([]byte))}}
			} else {
				infoDict.files = []TorrentFile{TorrentFile{string(info["name"].([]uint8)), uint64(info["length"].(int64)), ""}}
			}
			infoDict.totalLength = uint64(info["length"].(int64))
		}
	}

	if info["name"] != nil { // we have to do this even if there's a files dict, because some ambitious go-getters put both name and files fields in their infodicts (in direct violation of BEP 3)
		infoDict.name = string(info["name"].([]byte))
	}
	if info["md5sum"] != nil {
		infoDict.md5sum = string(info["md5sum"].([]byte))
	}

	// split the bytestring of concatenated 20-byte SHA1 hashes into a list of individual SHA1 hashes
	if piecesBytestringRaw := info["pieces"]; piecesBytestringRaw != nil {
		piecesBytestring := info["pieces"].([]uint8)
		if len(piecesBytestring)%20 != 0 {
			panic("Length of pieces section in torrent file is corrupted. (has a length that isn't a multiple of 20)")
		}

		nPieces := len(piecesBytestring) / 20
		infoDict.pieceHashes = make([][]byte, nPieces)
		for a := 0; a < nPieces; a++ {
			infoDict.pieceHashes[a] = piecesBytestring[20*a : 20*(a+1)]
		}
		return true
	}
	return false
}

func filePathListToString(filePathList []interface{}) string {
	// []byte casts is safe since this comes from an info dict that is sha-1 guaranteed to be right
	if len(filePathList) == 1 {
		return string(filePathList[0].([]byte))
	}

	var filePathString strings.Builder
	filePathString.WriteString(string(filePathList[0].([]byte)))
	for _, item := range filePathList[1:] {
		filePathString.WriteByte(byte('/'))
		filePathString.WriteString(string(item.([]byte)))
	}

	return filePathString.String()
}

func (data *TorrentData) buildTrackerRequest(peerID [20]byte, port uint16) string {
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
