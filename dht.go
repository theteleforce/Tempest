package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"time"

	bencode "github.com/IncSW/go-bencode"
)

// Constants //

// client stuff //

// DHTRoutingTablePath is the path of the routing table (or where the routing table will be)
const DHTRoutingTablePath = "dht/routing_table"

//DHTBootstrapPath is the path to a file of bootstrap DHT nodes (i.e. nodes that link us to other nodes)
const DHTBootstrapPath = "dht/bootstrap_nodes"

//  communication stuff //

// AddNodesChannelSize is the maximum backlog of node lists to be added (values added to the channel while it's full will be discarded)
const AddNodesChannelSize = 50

// DHTPeerTimeout is the number of seconds to wait before failing a DHT connection
const DHTPeerTimeout = 3 * time.Second

// DHTMaxPeers is the maximum number of peer *addresses* we fetch from DHT (not all of these will turn into peers)
const DHTMaxPeers = 205 // accept waaaay more than the actual number of peers needed, since most of them won't connect

// GetPeersTimeout is how long to wait while fetching peers over the DHT before just giving up and returning
const GetPeersTimeout = 10 * time.Second

// MaxDHTDiscoverDepth is the maximum number of times we recurse in find_node
const MaxDHTDiscoverDepth = 4

// MaxDHTSearchDepth is the maximum number of times we follow nodes pointing us to new nodes before we give up
const MaxDHTSearchDepth = 6 // could probably be finetuned

// NodeQualityDuration is the time after which we communicate with a node that we still consider that node 'reliable' (i.e., we don't have to ping it before requesting data from it)
const NodeQualityDuration = -15 * time.Minute // fixed per protocol, negative so that it goes backwards when we add it to time.Now()

// NodeStatusGood implies that a node is healthy and can be returned to DHT queries
const NodeStatusGood = 0

// NodeStatusQuestionable implies that a node hasn't been checked in longer than NodeQualityDuration, and should be pinged again before using or returning to DHT queries
const NodeStatusQuestionable = 1

// NodeStatusBad implies that a node isn't responding to pings, and should be replaced at the next opportunity
const NodeStatusBad = 2

// NodePingRetries is the number of times we try to contact a node before marking it as bad
const NodePingRetries = 2

// DHT CLIENT //

// DHTClient is used to hold the routing table for DHT peer lookup
type DHTClient struct {
	addNodesChannel chan []*DHTNode
	buckets         []*DHTBucket
	ipcChannels     *DhtIPCChannels
	nodeID          []byte
	numBuckets      uint32
}

// DHTBucket is used to hold up to 8 nodeIDs that are close to each other
type DHTBucket struct {
	lowerBound []byte
	upperBound []byte
	nodes      [8]*DHTNode
	numNodes   uint8
}

// NodeResult is used by channels to return a node or an error
type NodeResult struct {
	node *DHTNode
	err  error
}

// NodeListResult is used by channels to return a node list or an error
type NodeListResult struct {
	nodeList []*DHTNode
	err      error
}

// NodeStatusResult is used by channels to return the index and status of a node
type NodeStatusResult struct {
	nodeIndex  uint8
	nodeStatus uint8
}

func createDHTClient(peerID []byte, channels *DhtIPCChannels) *DHTClient {
	var client DHTClient
	client.addNodesChannel = make(chan []*DHTNode, AddNodesChannelSize) // used to communicate with the goroutine that does all post-initialization editing of the routing table
	client.ipcChannels = channels
	client.nodeID = peerID

	return &client
}

func (client *DHTClient) start() { // starts the main loop for this client
	client.populateRoutingTable()            // before anything else, populate the routing table (from storage or from bootstrap)
	defer client.encodeAndSaveRoutingTable() // when this window closes, make sure to save the routing table

	for true { // then, loop until the app is closed...
		select { // ergo shitty priority control (more than one successful case in a select will execute in pseudorandom order, but default is only executed if every other case [at its level] fails)
		default: // top prio is requests from the torrent client, since that handles actual functionality and may be time-sensitive
			select {
			default: // second prio is requests from the gui, since they affect the user experience and everything below them (i.e. adding nodes) doesn't have real time limits
				select {
				default: // third prio is adding nodes; if there aren't any, sleep for a bit and check for work again
					time.Sleep(SubprocessSleepTime)
				case newNodes := <-client.addNodesChannel:
					client.handleAddNodes(newNodes)
				}
			case guiRequest := <-client.ipcChannels.incomingGuiRequestChannel:
				client.handleGuiRequest(guiRequest)
			}
		case torrentRequest := <-client.ipcChannels.incomingTorrentRequestChannel:
			client.handleTorrentRequest(torrentRequest)
		}
	}
}

func (client *DHTClient) handleGuiRequest(guiRequest *IPCRequest) { // placeholder
	switch guiRequest.functionCall {
	default:
		client.ipcChannels.outgoingGuiReplyChannel <- &IPCReply{guiRequest.messageID, nil, fmt.Errorf("no function call with that name exists")}
	}
}

func (client *DHTClient) handleTorrentRequest(torrentRequest *IPCRequest) {
	switch torrentRequest.functionCall {
	default:
		client.ipcChannels.outgoingTorrentReplyChannel <- &IPCReply{torrentRequest.messageID, nil, fmt.Errorf("no function call with that name exists")}
	case "getPeersFor":
		switch torrentRequest.args[0].(type) {
		default:
			client.ipcChannels.outgoingTorrentReplyChannel <- &IPCReply{torrentRequest.messageID, nil, fmt.Errorf("no function call with that name exists")}
		case []byte:
			println("!!!!!calling get peers for!!!!!")
			peers, err := client.getPeersFor(torrentRequest.args[0].([]byte))
			if err != nil {
				client.ipcChannels.outgoingTorrentReplyChannel <- &IPCReply{torrentRequest.messageID, nil, fmt.Errorf("call to getPeersFor failed: %s", err)}
			} else {
				client.ipcChannels.outgoingTorrentReplyChannel <- &IPCReply{torrentRequest.messageID, []interface{}{peers}, nil}
			}
		}
	}
}

func (client *DHTClient) handleAddNodes(newNodes []*DHTNode) {
	if newNodes != nil { // invalid nodes are simply discarded
		for _, newNode := range newNodes {
			client.addNode(newNode)
		}
	}
}

func (client *DHTClient) populateRoutingTable() {
	routingTableData, err := ioutil.ReadFile(DHTRoutingTablePath) // start by loading the routing table created in past sessions
	if err != nil {                                               // if we couldn't...
		if os.IsNotExist(err) { // ...and it's because there isn't one...
			err = client.bootstrapRoutingTable() // ...create one
			if err != nil {
				panic(fmt.Sprintf("Couldn't read DHT routing table, and creating a new one failed (%s)", err)) // if we couldn't create one, we can't do DHT, which is a big problem
			}
		} else {
			panic(fmt.Sprintf("Couldn't read existing DHT routing table (%s)", err)) // otherwise, we can't do DHT, which is a big problem
		}
	} else { // otherwise, if we opened the file alright, initialize the DHT client with that info
		client.decodeRoutingTable(routingTableData)
	}
}

/*
func (client *DHTClient) startAddNodesChannel() { // background routine that handles all adding of nodes to the routing table after bootstrapping
	for true {
		newNodes := <-client.addNodesChannel // block until there's nodes to add
		if newNodes == nil {                 // invalid nodes are simply discarded
			continue
		}
		for _, newNode := range newNodes {
			client.addNode(newNode)
		}
	}
}
*/

func (client DHTClient) bootstrapRoutingTable() error { // channel safe
	bootstrapNodesData, err := ioutil.ReadFile(DHTBootstrapPath)
	if err != nil { // if we need the bootstrap nodes and can't find them...
		return fmt.Errorf("no dht bootstrap nodes found (checked at %s)", DHTBootstrapPath) // ...then yeah, we can't do DHT
	}

	var bootstrapBucket DHTBucket // create the default bucket; the bootstrap nodes will go here
	bootstrapBucket.lowerBound = []byte{0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}
	bootstrapBucket.upperBound = []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}
	client.buckets = make([]*DHTBucket, 1024) // we expect to have at least 1024 buckets when we're done
	client.buckets[0] = &bootstrapBucket
	client.numBuckets = 1

	bootstrapNodeAddrs := bytes.Split(bootstrapNodesData, []byte("|"))      // connect to the bootstrap nodes and store their data
	bootstrapNodesChannel := make(chan NodeResult, len(bootstrapNodeAddrs)) // use a channel to hold these, since they'll be reported from goroutines anyway
	for _, nodeAddr := range bootstrapNodeAddrs {
		go pingDHTAddressConcurrent(bootstrapNodesChannel, string(nodeAddr), client.nodeID) // get the nodeID for all the bootstrap nodes, so we can start requesting new nodes closer to us
	}
	reportedNodes := 0
	for reportedNodes < len(bootstrapNodeAddrs) {
		result := <-bootstrapNodesChannel // block until we get a node from the pings
		fmt.Printf("%s\n", result.err)
		if result.err == nil { // silently ignore failed connections to bootstrap nodes; we'll do a check at the end to make sure we have at least one
			client.addNode(result.node)
		}
		reportedNodes++
	}
	if client.buckets[0].nodes[0] == nil { // only have to check the first bucket for nodes, since there are less than 8 bootstrap nodes
		panic("None of the bootstrap DHT nodes returned peers!")
	}

	discoveredNodesChannel := make(chan NodeListResult, client.buckets[0].numNodes) // each bootstrap node that responded will collect nodes below it into a slice and send them here
	defer close(discoveredNodesChannel)
	for a := 0; uint8(a) < client.buckets[0].numNodes; a++ { // this iterates over all bootstrap nodes, since there are less than 8, they're all in one bucket || cast is always safe because numNodes is always 8 or less
		go findCloseNodes(discoveredNodesChannel, client.buckets[0].nodes[a].routableAddress, client.nodeID, 0)
	}

	success := false
	validBootstrapNodes := client.buckets[0].numNodes // copy this value here so it doesn't change once we start adding new nodes
	for a := 0; uint8(a) < validBootstrapNodes; a++ { // cast is safe because numNodes is never bigger than 8
		result := <-discoveredNodesChannel // block until one of the recursion trees finding nodes is complete
		if result.err == nil {             // if this tree succesfully found nodes...
			success = true
			for _, node := range result.nodeList { // ...add them to our bucket (use client.addNode since we haven't started the addNodeChannel channel yet)
				client.addNode(node)
			}
		}
	}

	if !success {
		return fmt.Errorf("none of the bootstrap nodes returned nodes")
	}
	err = client.encodeAndSaveRoutingTable() // last step: save the routing table so we don't have to do this next time
	if err != nil {
		return fmt.Errorf("couldn't save routing table")
	}
	return nil
}

func (client *DHTClient) addNode(node *DHTNode) { // channel safe
	node.lastContactedTime = time.Now() // if we're just adding this node, it must be freshly contacted

	bucketIndex, bucket := client.getBucketFor(node.nodeID[:])
	for a := 0; uint8(a) < bucket.numNodes; a++ { // if the node is already in this bucket, just return | cast is safe since numNodes is never bigger than 8
		if bytes.Compare(node.nodeID[:], bucket.nodes[a].nodeID[:]) == 0 {
			return
		}
	}
	if bucket.numNodes != 8 { // if this bucket isn't full yet...
		bucket.nodes[bucket.numNodes] = node // ...insert the new node at the first open slot
		bucket.numNodes++
		return // and we're done!
	}

	foundNodeToReplace := false
	nodesPinged := 0
	nodeStatusChannel := make(chan NodeStatusResult, bucket.numNodes) // channel of (node index, node status) tuples for nodes to report if they haven't been checked in a while
	defer close(nodeStatusChannel)                                    // this can cause errors in the goroutines when they try to write to a closed channel, but we can just ignore them, since a node opening would've already been found
	for a := 0; uint8(a) < bucket.numNodes; a++ {                     // otherwise, try to find a bad node we can replace || cast is always safe because numNodes is always 8 or less
		if bucket.nodes[a].status == NodeStatusBad { // if this node is known to be garbage...
			bucket.nodes[a] = node // ...replace it with the new node
			foundNodeToReplace = true
		}
		// otherwise, get it to check (and update via pinging) its status; if it's bad, it'll return via nodeStatusChannel and we can replace it
		go node.getStatus(client.nodeID, uint8(a), nodeStatusChannel) // cast is safe because a will never be bigger than 8
		nodesPinged++
	}

	for nodesReported := 0; nodesReported < nodesPinged; nodesReported++ {
		result := <-nodeStatusChannel           // block until we get a status update from one of the nodes
		if result.nodeStatus == NodeStatusBad { // if we pinged this node and its host didn't respond,
			bucket.nodes[result.nodeIndex] = node // ...we can replace it with the new node
			foundNodeToReplace = true             // and we're done (but still have to wait until the rest of the nodes finish reporting, to avoid writing on closed channel)
		}
	}

	if !foundNodeToReplace { // if we haven't found a node to replace, create a new bucket and store the node recursively in whichever bucket it fits in
		client.splitBucket(bucketIndex) // so split the bucket into two parts of equal domain...
		client.addNode(node)            // ...and insert the node recursively, until there's a bucket that can fit the node
	}
}

func (client *DHTClient) getBucketFor(nodeID []byte) (uint32, *DHTBucket) { // since this is only called internally and with length-controlled arguments, no error checking here
	for a := uint32(0); a < client.numBuckets; a++ {
		if client.buckets[a].contains(nodeID) {
			return a, client.buckets[a]
		}
	}

	panic(fmt.Sprintf("Something's wrong with the routing table at %s (couldn't find a bucket for ID %s)", DHTRoutingTablePath, nodeID))
}

func (bucket *DHTBucket) contains(nodeID []byte) bool { // since this is only called internally and with length-controlled arguments, no error checking here
	return bytewiseGreaterThan(nodeID, bucket.lowerBound, true) && bytewiseGreaterThan(bucket.upperBound, nodeID, false) // if the nodeID is bigger than the lower bound and less than the upper bound, this is the right bucket
}

func bytewiseGreaterThan(bytesA []byte, bytesB []byte, orEqual bool) bool { // since this is only called internally and with length-controlled arguments, no error checking here
	for a := 0; a < 20; a++ {
		if bytesA[a] > bytesB[a] { // if this byte is larger in A than B, then A is larger, since these are big-endian encoded 'numbers'
			return true
		}
		if bytesB[a] > bytesA[a] { // likewise, if this byte is larger in B, then B is larger
			return false
		}
		// otherwise, the bytes are equal, so check the next one
	}
	return orEqual // if we've gotten here, the bytestrings are exactly equal, so return whether the call wanted equal bytestrings or not
}

func (node *DHTNode) getStatus(localNodeID []byte, nodeIndex uint8, statusChannel chan NodeStatusResult) { // channel safe
	if node.lastContactedTime.After(time.Now().Add(NodeQualityDuration)) { // if it's been less than 15 minutes since we last heard from this node, then it's still valid
		select {
		default:
		case statusChannel <- NodeStatusResult{nodeIndex, NodeStatusGood}: // tell the main routine if it still needs to know; if it doesn't, it already found a bad node to replace
		}
		return
	}
	for a := 0; a < NodePingRetries; a++ {
		_, err := pingDHTAddress(node.routableAddress, localNodeID)
		if err == nil { // if we could ping the node successfully...
			node.status = NodeStatusGood // ..then it's still valid
			statusChannel <- NodeStatusResult{nodeIndex, NodeStatusGood}
			return
		}
	}

	// if we've gotten here, none of our pings registered, so the node is bad
	node.status = NodeStatusBad
	statusChannel <- NodeStatusResult{nodeIndex, NodeStatusBad}
}

func (client *DHTClient) splitBucket(bucketIndex uint32) {
	bucketToSplit := client.buckets[bucketIndex]

	//client.numBuckets++
	client.buckets = append(client.buckets, nil)                                // make space for the new bucket
	for a := int(client.numBuckets /* - 1*/); a > /*=*/ int(bucketIndex); a-- { // move all the buckets above or equal to the bucket index one step to the right | cast to int to avoid a = 0; a-- => a = 2^32 - 1
		client.buckets[a /*+1*/] = client.buckets[a-1] // this isn't *too* time intensive, since these are just pointers
	}
	client.numBuckets++

	midpoint := bucketToSplit.findMidpoint()
	midpointMinusOne := bytewiseMinusOne(midpoint)
	var newBucket DHTBucket
	newBucket.lowerBound = bucketToSplit.lowerBound
	newBucket.upperBound = midpointMinusOne  // the new bucket covers the lower half of the old bucket's range
	client.buckets[bucketIndex] = &newBucket // insert the new bucket

	client.buckets[bucketIndex+1].lowerBound = midpoint
	// the old bucket's upper bound is still the same

	// now, split the old nodes between the old bucket and the new one
	newBucketIndex := 0
	oldBucketIndex := 0
	for a := 0; uint8(a) < bucketToSplit.numNodes; a++ { // cast is always safe because numNodes is always 8 or less
		relevantNodePointer := bucketToSplit.nodes[a]
		if bytewiseGreaterThan(relevantNodePointer.nodeID[:], midpoint, true) { // if this node is bigger than or equal to the midpoint, it belongs in the old (upper) bucket
			client.buckets[bucketIndex+1].nodes[oldBucketIndex] = relevantNodePointer
			oldBucketIndex++
		} else {
			client.buckets[bucketIndex].nodes[newBucketIndex] = relevantNodePointer
			newBucketIndex++
		}
	}
	client.buckets[bucketIndex].numNodes = uint8(newBucketIndex)
	client.buckets[bucketIndex+1].numNodes = uint8(oldBucketIndex)

	// finally, remove the old nodes from the old bucket
	for a := oldBucketIndex; a < 8; a++ {
		bucketToSplit.nodes[a] = nil
	}
}

func (bucket *DHTBucket) findMidpoint() []byte {
	midpoint := make([]byte, 20)
	for a := 19; a >= 0; a-- {
		digitSum := uint16(bucket.lowerBound[a]) + uint16(bucket.upperBound[a]) // have to cast to prevent shit like 128 + 128 = 0
		if digitSum%2 == 1 && a != 19 {                                         // if the number is odd and this isn't the first digit (odd first digits are rounded down, i.e. (127 + 128)/2 = 127)
			if midpoint[a+1] > 127 {
				midpoint[a] = byte((digitSum / 2) + 1) // note this is never an overflow, since 255 + 255 is even and (255 + 254) / 2 = 254
			} else {
				midpoint[a] = byte(digitSum / 2)
			}
			midpoint[a+1] += 128 // by the same principle that 38 / 2 = (15 + 4), add half the base to the digit behind this one
		} else { // if this is the first digit, or the sum was even...
			midpoint[a] = byte(digitSum / 2) // ...the midpoint digit is just half the sum
		}
	}

	return midpoint
}

func bytewiseMinusOne(nodeID []byte) []byte {
	ret := make([]byte, 20)
	copy(ret, nodeID)
	for a := 19; a >= 0; a-- {
		if ret[a] == 0 {
			ret[a] = 255
		} else {
			ret[a]--
			return ret
		}
	}

	// if we've gotten this far, we've tried to split a bucket with a midpoint of zero
	panic(fmt.Sprintf("something is irreversably doomed in your routing table; probably delete it and restart (routing table path relative to executable: %s)", DHTRoutingTablePath))
}

func (client *DHTClient) encodeAndSaveRoutingTable() error {
	encodedTable := make([]byte, 4)
	binary.BigEndian.PutUint32(encodedTable, client.numBuckets) // store the number of buckets in the first 4 bytes | COULD LEAD TO CORRUPTION if there are ever more than 2^32 buckets

	for a := uint32(0); a < client.numBuckets; a++ {
		encodedTable = append(encodedTable, client.buckets[a].encode()...)
	}

	if _, err := os.Stat(DHTRoutingTablePath); (err != nil) && (!os.IsNotExist(err)) { // try to delete the old routing table, if it's there | this is inefficient as FUCK but deadlines are approaching
		err := os.Remove(DHTRoutingTablePath)
		if err != nil {
			return err
		}
	}

	// if we're here, then we're free to create and write to the routing table
	fp, err := os.Create(DHTRoutingTablePath)
	if err != nil {
		return err // if we can't create the routing table, just fail and try to write it next time
	}

	_, err = fp.Write(encodedTable)
	if err != nil {
		return err // if we can't create the routing table, just fail and try to write it next time
	}
	return nil
}

func (bucket *DHTBucket) encode() []byte {
	encodedBucket := make([]byte, 41)           // will be extended by every node
	copy(encodedBucket, bucket.lowerBound)      // first 20 bytes are the lower bound
	copy(encodedBucket[20:], bucket.upperBound) // second 20 bytes are the upper bound
	encodedBucket[40] = byte(bucket.numNodes)   // 41st byte is the number of nodes

	for a := 0; uint8(a) < bucket.numNodes; a++ { // cast is safe, since numNodes is never bigger than 8
		encodedBucket = append(encodedBucket, bucket.nodes[a].encode()...)
	}

	return encodedBucket
}

func (node *DHTNode) encode() []byte {
	encodedNode := make([]byte, 26+len(node.routableAddress))
	copy(encodedNode, node.nodeID[:])                                                     // first 20 bytes of the node is the node ID
	encodedNode[20] = byte(node.status)                                                   // 21st byte is the status
	binary.BigEndian.PutUint32(encodedNode[21:25], uint32(node.lastContactedTime.Unix())) // 22nd through 26th bytes is the epoch time when we last contacted this node
	encodedNode[25] = byte(uint8(len(node.routableAddress)))                              // this is safe because routableAddresses are either xxx.xxx.xxx.xxx:xxxxx or one of the short bootstrap nodes
	copy(encodedNode[26:], node.routableAddress)                                          // replace node.routableAddress with []byte(node.routableAddress) if necessary; guaranteed to work but longer time + more space

	return encodedNode
}

func (client *DHTClient) decodeRoutingTable(routingTableData []byte) {
	numBuckets := binary.BigEndian.Uint32(routingTableData[:4])
	client.numBuckets = numBuckets

	dataIndex := 4
	client.buckets = make([]*DHTBucket, numBuckets)
	for a := 0; uint32(a) < numBuckets; a++ { // cast is safe since a will never be negative
		newDataIndex, nextBucket := decodeBucketAt(routingTableData, dataIndex) // decode the tables starting at index 4 of the data
		dataIndex = newDataIndex
		client.buckets[a] = nextBucket
	}
}

func decodeBucketAt(routingTableData []byte, dataIndex int) (int, *DHTBucket) {
	var decodedBucket DHTBucket
	decodedBucket.lowerBound = make([]byte, 20)
	decodedBucket.upperBound = make([]byte, 20)

	copy(decodedBucket.lowerBound, routingTableData[dataIndex:(dataIndex+20)])      // first 20 bytes are the lower bound
	copy(decodedBucket.upperBound, routingTableData[(dataIndex+20):(dataIndex+40)]) // second 20 bytes are the upper bound
	decodedBucket.numNodes = routingTableData[dataIndex+40]                         // 41st byte is the number of nodes

	dataIndex += 41                                      // set the data index to the start of the first encoded node
	for a := 0; uint8(a) < decodedBucket.numNodes; a++ { // cast is safe because a will never be bigger than 8
		dataIndex, decodedBucket.nodes[a] = decodeNodeAt(routingTableData, dataIndex)
	}

	return dataIndex, &decodedBucket
}

func decodeNodeAt(routingTableData []byte, dataIndex int) (int, *DHTNode) {
	var decodedNode DHTNode

	copy(decodedNode.nodeID[:], routingTableData[dataIndex:(dataIndex+20)])                                                       // first 20 bytes are the node ID
	decodedNode.status = routingTableData[dataIndex+20]                                                                           // 21st byte is the status
	decodedNode.lastContactedTime = time.Unix(int64(binary.BigEndian.Uint32(routingTableData[(dataIndex+21):(dataIndex+25)])), 0) // 22nd through 26th bytes are the epoch seconds since we last contacted the node

	routableAddressLength := int(uint8(routingTableData[dataIndex+25]))                                               // the 27th byte tells us how long the routable address is
	decodedNode.routableAddress = string(routingTableData[(dataIndex + 26):(dataIndex + 26 + routableAddressLength)]) // and the rest of the node is read from that

	return dataIndex + 26 + routableAddressLength, &decodedNode // set the new data index at the first byte after this node
}

// DHT COMMUNICATION //

// DHTNode represents a single peer in the DHT network
type DHTNode struct {
	lastContactedTime time.Time
	nodeID            [20]byte
	routableAddress   string // e.g. "127.0.0.1:6881", "router.utorrent.com"
	status            uint8
}

func pingDHTAddress(URI string, peerID []byte) ([]byte, error) {
	messageDict := make(map[string]interface{})
	populateRequestMessageDict(messageDict, "ping")

	requestDict := make(map[string]interface{})
	requestDict["id"] = peerID
	messageDict["a"] = requestDict

	response, err := sendDHTMessage(URI, messageDict)
	switch response.(type) { // ping responses to DHT are always decoded to []byte; if we got something else, there's a problem
	default:
		return nil, err
	case []byte:
		return response.([]byte), nil
	}
}

func pingDHTAddressConcurrent(nodesChannel chan NodeResult, URI string, peerID []byte) { // channel safe
	nodeID, err := pingDHTAddress(URI, peerID) // get the node ID by pinging
	if err != nil {
		nodesChannel <- NodeResult{nil, err}
		return
	}

	var newNode DHTNode // if everything went well, create the node object and push it back to the master
	newNode.lastContactedTime = time.Now()
	copy(newNode.nodeID[:], nodeID)
	newNode.routableAddress = URI
	newNode.status = NodeStatusGood
	nodesChannel <- NodeResult{&newNode, nil}
}

func findCloseNodes(nodesDiscoveredChannel chan NodeListResult, nodeAddress string, localNodeID []byte, recursionDepth int) { // channel safe
	messageDict := make(map[string]interface{})
	populateRequestMessageDict(messageDict, "find_node")

	requestDict := make(map[string]interface{})
	requestDict["id"] = localNodeID
	requestDict["target"] = localNodeID // find nodes that are as close as possible to our node
	messageDict["a"] = requestDict

	nodePointersListInterface, err := sendDHTMessage(nodeAddress, messageDict)
	if err != nil { // if we couldn't contact this node...
		fmt.Println("couldn't connect to node at recursion depth ", recursionDepth)
		nodesDiscoveredChannel <- NodeListResult{nil, fmt.Errorf("couldn't find nodes: %s", err)} // prematurely shut down this branch
		return
	}
	nodePointersList := nodePointersListInterface.([]*DHTNode)

	// if we successfully got a list of nodes, there are three possibilities:
	recursionDepth++
	if (len(nodePointersList) == 1) || (recursionDepth == MaxDHTDiscoverDepth) { // if we found the correct node or this was our last recursive step, just write to our return channel
		nodesDiscoveredChannel <- NodeListResult{nodePointersList, err}
	} else { // otherwise, create a new return channel and recurse this function on the nodes we discovered -- then, when we're done, write the full list to our return channel
		recursiveNodesDiscoveredChannel := make(chan NodeListResult, len(nodePointersList))

		for _, node := range nodePointersList {
			go findCloseNodes(recursiveNodesDiscoveredChannel, node.routableAddress, localNodeID, recursionDepth) // note that recursionDepth has been incremented
		}

		newNodePointers := make([]*DHTNode, 0) // this will be appended to by every returned goroutine, so length here is relatively unimportant
		for a := 0; a < len(nodePointersList); a++ {
			result := <-recursiveNodesDiscoveredChannel
			if result.err == nil { // if the fetch was successful...
				newNodePointers = append(newNodePointers, result.nodeList...) // ...merge the returned node pointers into our master list
			}
		}

		if len(newNodePointers) == 0 { // if none of those 8 nodes returned new nodes, say so
			nodesDiscoveredChannel <- NodeListResult{nil, fmt.Errorf("no nodes found")}
		} else { // otherwise, return our node master list
			nodesDiscoveredChannel <- NodeListResult{newNodePointers, nil}
		}
	}
}

func (client *DHTClient) getPeersFor(infoHash []byte) ([]PeerAddress, error) {
	println("!!!!!starting getPeersFor!!!!!")
	for startingNodesTried := uint64(0); startingNodesTried < client.getNumGoodNodes(); startingNodesTried++ {
		closestNode := client.findClosestNodeToHash(infoHash) // find the node that's most likely to have peers for this hash (ignores bad nodes the previous iterations ruled out)
		fmt.Println("found closest node to start getPeersFor on")
		peerAddresses, err := client.startGetPeers(closestNode, infoHash)
		if err == nil {
			return peerAddresses, nil
		}
		fmt.Printf("dht node at %s was bad: %s \n", closestNode.routableAddress, err)
	}

	// if we've gotten here, none of our starting nodes replied, so this torrent can't use DHT for peers
	return nil, fmt.Errorf("dht couldn't find any peers for the torrent with info hash %s", string(infoHash))
}

func (client *DHTClient) getNumGoodNodes() uint64 {
	println("!!!!!starting getNumGoodNodes!!!!!")
	numGoodNodes := uint64(0)
	for a := uint32(0); a < client.numBuckets; a++ {
		for b := uint8(0); b < client.buckets[a].numNodes; b++ {
			if client.buckets[a].nodes[b].status == NodeStatusGood {
				numGoodNodes++
			}
		}
	}

	println("!!!!!finished getNumGoodNodes!!!!!")
	return numGoodNodes
}

func (client *DHTClient) startGetPeers(startingNode *DHTNode, infoHash []byte) ([]PeerAddress, error) {
	fmt.Println("!!!!!running startGetPeers!!!!!")
	addressesOrNewNodes, err := startingNode.sendGetPeers(client.nodeID, infoHash)
	if err != nil {
		return nil, fmt.Errorf("couldn't connect to closest dht node: %s", err) // if sending getPeers to the first node failed, bail out
	}
	switch addressesOrNewNodes.(type) {
	default: // if somehow we get something that isn't either of the intended returned types, bail out
		println("unknown response to startGetPeers; aborting")
		return nil, fmt.Errorf("got unknown return type")
	case []PeerAddress: // if this node had the peer addresses, return them and exit
		println("found peers on first dht node")
		return addressesOrNewNodes.([]PeerAddress), nil
	case []*DHTNode: // if this node just gave us more nodes, store them and try to get peers from them instead
		client.addNodes(addressesOrNewNodes.([]*DHTNode))

		peerAddresses := make([]PeerAddress, 0)
		peerAddressesChannel := make(chan []PeerAddress, DHTMaxPeers)
		//peersRemainingChannel := make(chan int, 1) // keeps track of how many peer addresses we still need until we're finished
		defer close(peerAddressesChannel)
		//defer close(peersRemainingChannel)

		for _, node := range addressesOrNewNodes.([]*DHTNode) { // first, issue the requests to those peers for those nodes
			go node.getPeersConcurrent(client.nodeID, infoHash, peerAddressesChannel /*, peersRemainingChannel*/, client.addNodesChannel, 0)
		}
		//timeoutGetPeers(peerAddressesChannel, peersRemainingChannel) // set a timeout to return whatever peers we have if we aren't done fetching them yet

		peersReturned := 0
		//peersRemainingChannel <- DHTMaxPeers // at every point the channel holds the number of peers it still needs, so the goroutines know how many to write | also blocks until the first goroutine finds peers
		//for peersReturned < DHTMaxPeers {
		for a := 0; a < len(addressesOrNewNodes.([]*DHTNode)); a++ {
			newPeerAddresses := <-peerAddressesChannel // block until one of our subroutines returns
			if newPeerAddresses != nil {               // nil is returned by nodes that are dead ends
				peersReturned += len(newPeerAddresses)
				peerAddresses = append(peerAddresses, newPeerAddresses...)
			}
			/*
				for _, peerAddress := range newPeerAddresses {
					peerAddresses[peersReturned] = peerAddress // ...then add all the peers to the list...
					peersReturned++                            // ...update how many peers we have...
				}

				if peersReturned < DHTMaxPeers {
					peersRemainingChannel <- DHTMaxPeers - peersReturned // ...and tell the other goroutines how many peers we still need, if we still need any | this blocks until the next goroutine finds nodes
				}
			*/
		}

		println("!!!!!finished startGetPeers!!!!!")
		return peerAddresses, nil // once we have 50 or we've timed out, return (closing both channels in the process)
	}
}

func (node *DHTNode) getPeersConcurrent(peerID []byte, infoHash []byte, peerAddressesChannel chan []PeerAddress /* peersRemainingChannel chan int,*/, addNodesChannel chan []*DHTNode, recursionDepth int) {
	fmt.Printf("recursing getPeers to node %s on recursion level %d\n", node.routableAddress, recursionDepth)
	addressesOrNewNodes, err := node.sendGetPeers(peerID, infoHash)
	if err != nil { // if it failed, bail out of this branch
		fmt.Printf("send to node %s on recursion level %d failed; dropping branch\n", node.routableAddress, recursionDepth)
		peerAddressesChannel <- nil
		return
	}
	switch addressesOrNewNodes.(type) {
	default: // if somehow we get something that isn't either of the intended returned types, bail out of this branch
		fmt.Printf("node %s on recursion level %d returned strange type; dropping branch\n", node.routableAddress, recursionDepth)
		peerAddressesChannel <- nil
	case []PeerAddress:
		fmt.Printf("node %s on recursion level %d returned peers!!!!\n", node.routableAddress, recursionDepth)
		/*peersRemaining, isOpen := <-peersRemainingChannel // get the number of peers that we still need, waiting for any other goroutine that might be writing peers to finish before we do so
		if isOpen {                                       // if the channel is closed, we already have all the peers we need
			// otherwise, write as many peers as needed to the channel
			if peersRemaining <= len(addressesOrNewNodes.([]PeerAddress)) { // if we got more peer addresses than we still need...
				peerAddressesChannel <- addressesOrNewNodes.([]PeerAddress)[:peersRemaining] // ...return exactly as many as we need and exit
			} else {
				peerAddressesChannel <- addressesOrNewNodes.([]PeerAddress) // otherwise, return all the addresses we have and exit (the client updates peersRemainingChannel for us)
			}
		}*/
		peerAddressesChannel <- addressesOrNewNodes.([]PeerAddress)
	case []*DHTNode:
		fmt.Printf("node %s on recursion level %d returned more nodes\n", node.routableAddress, recursionDepth)
		recursionDepth++
		if recursionDepth == MaxDHTSearchDepth { // if we've already recursed the maximum number of times, just give up on this tree of nodes
			fmt.Printf("reached max recursion depth %d on node %s; returning\n", recursionDepth, node.routableAddress)
			peerAddressesChannel <- nil
			return
		}

		peerAddresses := make([]PeerAddress, 0)
		newPeerAddressesChannel := make(chan []PeerAddress, DHTMaxPeers)
		for _, node := range addressesOrNewNodes.([]*DHTNode) { // otherwise, try to fetch peers from the nodes this node returned
			go node.getPeersConcurrent(peerID, infoHash, newPeerAddressesChannel /*, peersRemainingChannel*/, addNodesChannel, recursionDepth) // note that recursionDepth has been incremented
		}
		for a := 0; a < len(addressesOrNewNodes.([]*DHTNode)); a++ { // get new peer addresses from all of our subroutines
			newPeerAddresses := <-newPeerAddressesChannel // block until one of our subroutines returns
			if newPeerAddresses != nil {                  // nil is returned by nodes that are dead ends
				peerAddresses = append(peerAddresses, newPeerAddresses...)
			}
		}

		peerAddressesChannel <- peerAddresses                       // once we've accumulated all the peer addresses from our children, pass the results up to our parent
		addNodes(addressesOrNewNodes.([]*DHTNode), addNodesChannel) // then add the nodes we got to our routing table (discarding them if there's already a ton of node lists waiting)
	}
}

func (client *DHTClient) addNodes(nodesToAdd []*DHTNode) {
	addNodes(nodesToAdd, client.addNodesChannel)
}

func addNodes(nodesToAdd []*DHTNode, addNodesChannel chan []*DHTNode) {
	select {
	case addNodesChannel <- nodesToAdd: // if there's space to write to the channel, do it
	default: // otherwise, discard these nodes
	}
}

func (node *DHTNode) sendGetPeers(peerID []byte, infoHash []byte) (interface{}, error) {
	messageDict := make(map[string]interface{})
	populateRequestMessageDict(messageDict, "get_peers")

	queryDict := make(map[string]interface{})
	queryDict["id"] = peerID
	queryDict["info_hash"] = infoHash

	messageDict["a"] = queryDict

	retValue, retError := sendDHTMessage(node.routableAddress, messageDict)
	if retError != nil { // if we couldn't send the message, mark the node as bad
		node.status = NodeStatusBad
	} else {
		node.status = NodeStatusGood
	}
	return retValue, retError
}

/*
func timeoutGetPeers(peerAddressesChannel chan []PeerAddress, peersRemainingChannel chan int) { // automatically shuts down getPeers if it takes too long by filling the rest of the peer address list with nil
	time.Sleep(GetPeersTimeout)

	select {
	case peersRemaining, isOpen := <-peersRemainingChannel: // if there aren't any goroutines writing peer addresses...
		if !isOpen { // if we've already finished getting peers by the time this timeout triggers, just exit
			return
		}
		emptyPeers := make([]PeerAddress, peersRemaining)
		for a := 0; a < peersRemaining; a++ { // ...fill the remaining slots with nil, forcing getPeers to return whatever peers it already has
			emptyPeers[a] = PeerAddress{nil, 0}
		}
		peerAddressesChannel <- emptyPeers
	default:
		time.Sleep(time.Second / 10) // otherwise, if the channel is still open but in use, wait for whoever's using it to finish
	}
}
*/

func populateRequestMessageDict(messageDict map[string]interface{}, queryType string) {
	messageDict["y"] = "q"       // this message is of type query...
	messageDict["q"] = queryType // ...and this is the query type

	transactionID := make([]byte, 2) // generate a random 2-byte transaction ID
	rand.Read(transactionID)
	messageDict["t"] = transactionID
}

func sendDHTMessage(nodeURI string, messageDict map[string]interface{}) (interface{}, error) {
	udpConnection, err := net.DialTimeout("udp", nodeURI, UDPTrackerTimeout)
	if err != nil {
		return nil, fmt.Errorf("couldn't create UDP connection: %s", err)
	}
	defer udpConnection.Close() // close this connection once we're done, whether we get a response or an error

	messageBytes, err := bencode.Marshal(messageDict)
	if err != nil {
		return nil, fmt.Errorf("something went wrong while bencoding a DHT message: %s", err)
	}

	_, err = udpConnection.Write(messageBytes)
	if err != nil {
		return nil, fmt.Errorf("unable to send message to DHT peer: %s", err)
	}

	response := make([]byte, 1562)                            // 1562 is enough to hold 54 new nodes or 234 new peers
	udpConnection.SetDeadline(time.Now().Add(DHTPeerTimeout)) // set timeout for read (no defer to reset the timeout is necessary, since this is the only time we'll use this connection)
	_, err = udpConnection.Read(response)
	if err != nil {
		return nil, fmt.Errorf("couldn't recieve response from DHT node: %s", err)
	}

	responseDictPrototype, err := bencode.Unmarshal(response)
	if err != nil {
		return nil, fmt.Errorf("couldn't decode response from DHT node: %s", err)
	}

	switch responseDictPrototype.(type) {
	default: // this should always be a dict, so if it isn't, something went wrong in translation
		return nil, fmt.Errorf("couldn't decode response from DHT node: %s", err)
	case map[string]interface{}: // if it is, continue with the program
	}
	responseDict := responseDictPrototype.(map[string]interface{}) // also this is safe now

	if responseDict["y"] == nil { // if we couldn't do the conversion, something went wrong in translation
		return nil, fmt.Errorf("response from DHT node was missing response type (key 'y')")
	}

	responseCode := responseDict["y"].([]byte)
	if responseCode[0] != byte('r') {
		if responseCode[0] == byte('e') { // if the node returned an error
			errorInfo := responseDict["e"].([]interface{}) // pass along the information
			return nil, fmt.Errorf("the DHT node returned error code %d: %s", errorInfo[0].(int64), errorInfo[1].([]byte))
		}
		return nil, fmt.Errorf("the DHT returned an unknown response type: %s", responseCode)
	}

	// if we've gotten this far, the peer returned a 'response'
	if bytes.Compare(responseDict["t"].([]byte), messageDict["t"].([]byte)) != 0 {
		return nil, fmt.Errorf("the DHT node returned a different transactionID than the one we sent")
	}

	// token isn't recorded yet, but could easily be by checking out messageDict["token"].([]byte)

	// if we've gotten this far, there was a valid response, so choose the return type based on what the original request was
	// if the response has a 'values' field, it's given us a list of peers to download our torrent from
	responseData := responseDict["r"].(map[string]interface{})
	switch messageDict["q"] {
	default:
		return nil, fmt.Errorf("invalid query type %s", messageDict["q"])
	case "find_node":
		return parseFindNodeResponse(responseData)
	case "get_peers":
		return parseGetPeersResponse(responseData)
	case "ping":
		return parsePingResponse(responseData)
	}
}

func parseFindNodeResponse(responseData map[string]interface{}) ([]*DHTNode, error) {
	switch responseData["nodes"].(type) {
	default:
		return nil, fmt.Errorf("find_node call didn't return a list of nodes")
	case []byte:
		return decodeNodes(responseData["nodes"].([]byte)) // if the response had a list of compacted nodes, as it should, decode and return them
	}
}

func parseGetPeersResponse(responseData map[string]interface{}) (interface{}, error) {
	if responseData["values"] != nil { // if the node gave us a list of peer addresses,
		switch responseData["values"].(type) {
		default:
			return nil, fmt.Errorf("the DHT node returned incorrectly formatted peer data in get_peers")
		case []interface{}:
			peerDataList := responseData["values"].([]interface{})
			peerAddresses := make([]PeerAddress, len(peerDataList))

			for a, addressDataString := range peerDataList { // assume the data strings are 6 bytes and correctly formatted
				peerAddresses[a].IP = net.IP((addressDataString.([]byte))[:4])
				peerAddresses[a].port = binary.BigEndian.Uint16((addressDataString.([]byte))[4:])
			}
			return peerAddresses, nil
		}
	}

	if responseData["nodes"] != nil { // otherwise, if the response has a 'nodes' field, it's giving us new nodes to ask for peers
		switch responseData["nodes"].(type) {
		default:
			return nil, fmt.Errorf("the DHT node returned incorrectly formatted node data in get_peers")
		case []byte:
			return decodeNodes(responseData["nodes"].([]byte))
		}
	}

	// if we've gotten this far, the response had neither new nodes nor peers, so something is clearly wrong
	return nil, fmt.Errorf("the DHT node didn't return peers or new nodes")
}

func decodeNodes(newNodesData []byte) ([]*DHTNode, error) {
	if (len(newNodesData) % 26) != 0 { // peers are encoded as 26-byte strings; 20 for their ID, 4 for their IP, 2 for their port
		return nil, fmt.Errorf("the DHT peer returned incorrectly formatted nodes")
	}

	numNodes := len(newNodesData) / 26
	dhtNodes := make([]*DHTNode, numNodes)
	nodeBytestringIndex := 0
	for nodeIndex := 0; nodeIndex < numNodes; nodeIndex++ {
		var newNode DHTNode
		copy(newNode.nodeID[:], newNodesData[nodeBytestringIndex:(nodeBytestringIndex+20)])
		newNode.routableAddress = ipAndPortToString(newNodesData[(nodeBytestringIndex + 20):(nodeBytestringIndex + 26)])
		newNode.lastContactedTime = time.Now()

		nodeBytestringIndex += 26
		dhtNodes[nodeIndex] = &newNode
	}
	return dhtNodes, nil
}

func parsePingResponse(responseData map[string]interface{}) ([]byte, error) {
	if responseData["id"] != nil {
		switch responseData["id"].(type) {
		default:
			return nil, fmt.Errorf("incorrectly formatted ID in ping response (expecting hex string)")
		case []byte:
			return hex.DecodeString(string(responseData["id"].([]byte)))
		}
	}
	return nil, fmt.Errorf("no id in ping response")
}

func (client *DHTClient) findClosestNodeToHash(infoHash []byte) *DHTNode {
	var closestNode *DHTNode
	var closestXOR []byte

	if client.numBuckets == 1 && (client.buckets[0].numNodes == 0) {
		panic(fmt.Sprintf("Tried to do a DHT lookup without any known DHT nodes (somehow)! Try deleting your routing table at %s and restarting Tempest.", DHTRoutingTablePath))
	}

	for a := uint32(0); a < client.numBuckets; a++ { // set the default closest node to the first one we have
		if client.buckets[a].numNodes != 0 {
			closestNode = client.buckets[a].nodes[0]
			closestXOR = bytewiseXOR(client.buckets[a].nodes[0].nodeID[:], infoHash)
			break
		}
	}

	for a := uint32(0); a < client.numBuckets; a++ { // check all nodes to see if any are closer; if they are, store that instead
		for b := 0; uint8(b) < client.buckets[a].numNodes; b++ { // cast is always safe because numNodes is always 8 or less
			relevantNodePointer := client.buckets[a].nodes[b]
			if relevantNodePointer.status == NodeStatusGood { // only check nodes that might return something
				for c, nodeIDByte := range relevantNodePointer.nodeID {
					if (infoHash[c] ^ nodeIDByte) < closestXOR[c] { // if this XOR is going to be smaller than the previous closest XOR
						closestNode = relevantNodePointer
						closestXOR = bytewiseXOR(relevantNodePointer.nodeID[:], infoHash) // would be slightly faster to start with the bytes we've already XOR'd, but we'd also have to allocate space for the XOR on each node and frankly fuck that
						break
					}
				}
			}
		}
	}

	return closestNode
}

func bytewiseXOR(bytesA []byte, bytesB []byte) []byte { // called only internally, so no error handling
	ret := make([]byte, len(bytesA))
	for a := 0; a < len(bytesA); a++ {
		ret[a] = bytesA[a] ^ bytesB[a]
	}
	return ret
}

func ipAndPortToString(addrData []byte) string { // callled only internally and with slices of 6 bytes, so no error handling
	return fmt.Sprintf("%s:%d", net.IP(addrData[:4]), binary.BigEndian.Uint16(addrData[4:]))
}
