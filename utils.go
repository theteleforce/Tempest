package main

import (
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"

	bencode "github.com/IncSW/go-bencode"
)

// Constants //

// ProtocolName is the name of the torrent protocol that gets passed with every handshake
const ProtocolName = "BitTorrent protocol"

// ProtocolLength is the length of PROTOCOL_NAME, passed with every handshake
const ProtocolLength byte = 19

// "Constants" but they aren't constant datatypes //

// InfoDictStartBytes are the bytes that start an info dict
var InfoDictStartBytes = []byte{52, 58, 105, 110, 102, 111, 100} // 4:infod

// InfoDictEndBytes are the bytes that end an info dict (if it ends in a way other than file termination)
var InfoDictEndBytes = []byte{101, 56, 58, 117, 114, 108, 45, 108, 105, 115, 116} // e8:url-list

// Structs //

// OrderedDict is an ordered "dict" where keys and values are equal indices; it's required because bencoded dicts have to be byte-ordered by key
type OrderedDict struct {
	keys   []string
	values []interface{}
}

// Functions //

// Safe file creation that panics if it fails
func safeCreateFile(filepath string) *os.File {
	file, err := os.Create(filepath)
	if err != nil {
		panic(fmt.Sprintf("Failed to create file %s: %s", filepath, err))
	}
	return file
}

func safeMarshal(data interface{}) []byte {
	encodedData, err := bencode.Marshal(data)
	if err != nil {
		panic(fmt.Sprintf("Error during bencoding of the following object: %b", data)) // not sure how to meaningfully print interfaces?
	}
	return encodedData
}

func safeReadFile(filepath string) []byte {
	fileContents, err := ioutil.ReadFile(filepath)
	if err != nil {
		panic(fmt.Sprintf("Error when reading file %s: %s", filepath, err))
	}
	return fileContents
}

func safeUnmarshal(bencodedData []byte) interface{} {
	decodedData, err := bencode.Unmarshal(bencodedData)
	if err != nil {
		panic(fmt.Sprintf("Error during de-bencoding of the following bytes: %b", bencodedData))
	}
	return decodedData
}

func safeWrite(file *os.File, data []byte) {
	_, err := file.Write(data)
	if err != nil {
		panic(fmt.Sprintf("Failed writing to file %s!", file.Name()))
	}
}

// httpHandler.go //

func safeReadHTTPResponse(response *http.Response) []byte {
	responseBody, err := ioutil.ReadAll(response.Body)
	if err != nil {
		panic(fmt.Sprintf("Error when reading response from %s%s", response.Request.URL.Host, response.Request.URL.Path))
	}
	return responseBody
}

func safeURLParse(rawURL string) *url.URL {
	parsedURL, err := url.Parse(rawURL)
	if err != nil {
		panic(fmt.Sprintf("Error when attempting to parse the following URL: %s", rawURL))
	}
	return parsedURL
}

func makeOrderedDict() *OrderedDict {
	var orderedDict OrderedDict
	orderedDict.keys = make([]string, 0)
	orderedDict.values = make([]interface{}, 0)
	return &orderedDict
}

func (orderedDict *OrderedDict) addKeyValuePair(key string, value interface{}) {
	orderedDict.keys = append(orderedDict.keys, key)
	orderedDict.values = append(orderedDict.values, value)
}

func bencodeOrderedDict(orderedDict *OrderedDict) ([]byte, error) { // since dicts in bittorrent have to be sorted by the byte order of keys, encoding actual dicts fails nondeterministically
	// possibly as a joke
	bencodedDict := bytes.NewBufferString("d")
	for a, key := range orderedDict.keys {
		bencodedDict.WriteString(fmt.Sprintf("%d:%s", len(key), key))

		value := orderedDict.values[a]
		switch value.(type) {
		default:
			bencoded, err := bencode.Marshal(orderedDict.values[a])
			if err != nil {
				return nil, err
			}
			bencodedDict.Write(bencoded) // if it's not a dict, it's fine to bencode with the (faster) library
		case *OrderedDict:
			bencoded, err := bencodeOrderedDict(value.(*OrderedDict))
			if err != nil {
				return nil, err
			}
			bencodedDict.Write(bencoded)
		case []*OrderedDict:
			bencoded, err := bencodeOrderedDictList(value.([]*OrderedDict))
			if err != nil {
				return nil, err
			}
			bencodedDict.Write(bencoded)
		}
	}
	bencodedDict.WriteString("e") // end the dict with a terminator

	return bencodedDict.Bytes(), nil
}

func bencodeOrderedDictList(orderedDictList []*OrderedDict) ([]byte, error) {
	bencodedList := bytes.NewBufferString("l")

	for _, orderedDict := range orderedDictList {
		bencodedDict, err := bencodeOrderedDict(orderedDict)
		if err != nil {
			return nil, err
		}
		bencodedList.Write(bencodedDict)
	}

	bencodedList.WriteString("e") // end the dict with a terminator
	return bencodedList.Bytes(), nil
}

func stringListToInterfaceList(strings []string) []interface{} {
	ret := make([]interface{}, len(strings))
	for a, str := range strings {
		ret[a] = str
	}
	return ret
}

func getInfoHash(torrentDataBytes []byte) [20]byte {
	infoDictStartIndex := 0
	infoDictEndIndex := len(torrentDataBytes) - 1 // by default, assume the info dict ends the torrent file (since it usually does)

	for a := 0; a < len(torrentDataBytes)-11; a++ {
		if (infoDictStartIndex == 0) && (bytes.Compare(torrentDataBytes[a:(a+7)], InfoDictStartBytes) == 0) {
			infoDictStartIndex = a + 6 // include the last byte of the 'header', the 'd' tag
		} else if (infoDictStartIndex != 0) && (bytes.Compare(torrentDataBytes[a:(a+11)], InfoDictEndBytes) == 0) {
			infoDictEndIndex = a + 1 // if this is the end, it starts with the 'e' byte, which is included in the infodict hash
			break
		}
	}
	if infoDictStartIndex == 0 {
		panic("Malformed torrent file: no info dict")
	}
	test := torrentDataBytes[infoDictStartIndex:infoDictEndIndex]
	print(test)
	return sha1.Sum(torrentDataBytes[infoDictStartIndex:infoDictEndIndex])
}

func encodePiecesDict(piecesDict map[uint32][]byte) []byte {
	totalLength := 0
	for _, data := range piecesDict {
		totalLength += (8 + len(data))
	}

	dataIndex := 0
	encodedDict := make([]byte, totalLength)
	for index, data := range piecesDict {
		binary.BigEndian.PutUint32(encodedDict[dataIndex:(dataIndex+4)], index)
		binary.BigEndian.PutUint32(encodedDict[(dataIndex+4):(dataIndex+8)], uint32(len(data))) // cast is safe because data is always less than 2,000,000,000
		copy(encodedDict[(dataIndex+8):], data)
		dataIndex += (8 + len(data))
	}

	return encodedDict
}

func decodePiecesDict(filename string) (map[uint32][]byte, error) {
	piecesDict := make(map[uint32][]byte)

	encodedDict, err := ioutil.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("couldn't read stored pieces dict at %s: %s", filename, err)
	}

	dataIndex := 0
	for dataIndex < len(encodedDict) {
		pieceIndex := binary.BigEndian.Uint32(encodedDict[dataIndex:(dataIndex + 4)])
		pieceLength := int(binary.BigEndian.Uint32(encodedDict[(dataIndex + 4):(dataIndex + 8)])) // cast is safe until pieces get above 2^31 - 1 in size; hopefully bittorrent protocol prevents that
		pieceData := make([]byte, pieceLength)
		copy(pieceData, encodedDict[(dataIndex+8):])
		piecesDict[pieceIndex] = pieceData

		dataIndex += (8 + pieceLength)
	}

	return piecesDict, nil
	/*fp, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("couldn't open stored file chunk at %s: %s", filename, err)
	}
	defer fp.Close()

	fileReader := bufio.NewReader(fp)
	for true {
		pieceDataBytes := make([]byte, 4) // we use this buffer twice: first for the pieceIndex uint32, then for the pieceLength uint32
		_, err := fileReader.Read(pieceDataBytes)
		if err != nil {
			if err.Error() == "EOF" { // if we read an EOF where we expected the next chunk to start, we're done with this file
				return piecesDict, nil
			}
			return nil, fmt.Errorf("couldn't read index of a piece in chunk file %s: %s", filename, err)
		}
		pieceIndex := binary.BigEndian.Uint32(pieceDataBytes)

		_, err = fileReader.Read(pieceDataBytes)
		if err != nil {
			return nil, fmt.Errorf("couldn't read length of a piece in chunk file %s: %s", filename, err)
		}

		pieceData := make([]byte, binary.BigEndian.Uint32(pieceDataBytes)) // the second value we read was the length of the data in this piece in bytes
		_, err = fileReader.Read(pieceData)
		if err != nil {
			return nil, fmt.Errorf("couldn't read data of a piece in chunk file %s: %s", filename, err)
		}
		piecesDict[pieceIndex] = pieceData
	}

	panic("Something went catastrophically wrong in decodePiecesDict (just restart Tempest).")*/
}
