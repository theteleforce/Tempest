package main

import (
	"math/rand"
	"time"
)

func main() {
	startIPC()
	// torrent.downloadFromMagnetLink("magnet:?xt=urn:btih:acecd763048a7fbfd32c2ef0e40cb1aad5b69233&dn=Twin+Peaks+S03+1080p+10bit+AMZN+WEBRip+x265+HEVC+6CH-MRN&tr=udp%3A%2F%2Ftracker.leechers-paradise.org%3A6969&tr=udp%3A%2F%2Fzer0day.ch%3A1337&tr=udp%3A%2F%2Fopen.demonii.com%3A1337&tr=udp%3A%2F%2Ftracker.coppersurfer.tk%3A6969&tr=udp%3A%2F%2Fexodus.desync.com%3A6969")
	//torrent.downloadFromMagnetLink("magnet:?xt=urn:btih:D9EEC693A0C9D5DEA1B55C0AC0182CD1620D079B&dn=DOOM+%28v6.66%2FUpdate+9%2C+MULTi10%29+%5BFitGirl+Final+Repack%2C+Selective+Download+-+from+30.4+GB%5D&tr=udp%3A%2F%2F9.rarbg.to%3A2710%2Fannounce&tr=udp%3A%2F%2Feddie4.nl%3A6969%2Fannounce&tr=udp%3A%2F%2Fshadowshq.yi.org%3A6969%2Fannounce&tr=udp%3A%2F%2Fthetracker.org%3A80%2Fannounce&tr=udp%3A%2F%2Ftracker.coppersurfer.tk%3A6969&tr=udp%3A%2F%2Ftracker.leechers-paradise.org%3A6969&tr=udp%3A%2F%2Ftracker.opentrackr.org%3A1337&tr=udp%3A%2F%2Ftracker.piratepublic.com%3A1337%2Fannounce&tr=udp%3A%2F%2Ftracker.zer0day.to%3A1337&tr=udp%3A%2F%2F9.rarbg.com%3A2710%2Fannounce&tr=udp%3A%2F%2Fexplodie.org%3A6969%2Fannounce&tr=udp%3A%2F%2Ftracker1.wasabii.com.tw%3A6969%2Fannounce&tr=udp%3A%2F%2Ftracker.torrent.eu.org%3A451%2Fannounce&tr=udp%3A%2F%2Ftracker.cypherpunks.ru%3A6969%2Fannounce&tr=udp%3A%2F%2Ftracker.zer0day.to%3A1337%2Fannounce&tr=udp%3A%2F%2Ftracker.leechers-paradise.org%3A6969%2Fannounce&tr=udp%3A%2F%2Fcoppersurfer.tk%3A6969%2Fannounce")
	//decodePiecesDict("tmp\\661b2365e2226122339cf73197ea0a1d5a8ed392\\1292")
	println("hooray")
}

func startIPC() {
	torrentChannels, dhtChannels, guiChannels := createIPCChannels() // create the struct of channels that the three main processes (torrentClient, dhtClient, and gui) will use to talk to each other

	torrentClient := createTorrentClient(torrentChannels)
	dhtClient := createDHTClient(torrentClient.peerID[:], dhtChannels)
	println(guiChannels)

	go torrentClient.start()
	go dhtClient.start()

	messageID := make([]byte, 20)
	rand.Read(messageID)
	//torrentClient.ipcChannels.incomingGuiRequestChannel <- &IPCRequest{messageID, "downloadFromFile", []interface{}{"..\\resources\\archlinux-2020.02.01-x86_64.iso.torrent"}}
	torrentClient.ipcChannels.incomingGuiRequestChannel <- &IPCRequest{messageID, "downloadFromMagnetLink", []interface{}{"magnet:?xt=urn:btih:D9EEC693A0C9D5DEA1B55C0AC0182CD1620D079B&dn=DOOM+%28v6.66%2FUpdate+9%2C+MULTi10%29+%5BFitGirl+Final+Repack%2C+Selective+Download+-+from+30.4+GB%5D&tr=udp%3A%2F%2F9.rarbg.to%3A2710%2Fannounce&tr=udp%3A%2F%2Feddie4.nl%3A6969%2Fannounce&tr=udp%3A%2F%2Fshadowshq.yi.org%3A6969%2Fannounce&tr=udp%3A%2F%2Fthetracker.org%3A80%2Fannounce&tr=udp%3A%2F%2Ftracker.coppersurfer.tk%3A6969&tr=udp%3A%2F%2Ftracker.leechers-paradise.org%3A6969&tr=udp%3A%2F%2Ftracker.opentrackr.org%3A1337&tr=udp%3A%2F%2Ftracker.piratepublic.com%3A1337%2Fannounce&tr=udp%3A%2F%2Ftracker.zer0day.to%3A1337&tr=udp%3A%2F%2F9.rarbg.com%3A2710%2Fannounce&tr=udp%3A%2F%2Fexplodie.org%3A6969%2Fannounce&tr=udp%3A%2F%2Ftracker1.wasabii.com.tw%3A6969%2Fannounce&tr=udp%3A%2F%2Ftracker.torrent.eu.org%3A451%2Fannounce&tr=udp%3A%2F%2Ftracker.cypherpunks.ru%3A6969%2Fannounce&tr=udp%3A%2F%2Ftracker.zer0day.to%3A1337%2Fannounce&tr=udp%3A%2F%2Ftracker.leechers-paradise.org%3A6969%2Fannounce&tr=udp%3A%2F%2Fcoppersurfer.tk%3A6969%2Fannounce"}}
	//torrentClient.ipcChannels.incomingGuiRequestChannel <- &IPCRequest{messageID, "downloadFromMagnetLink", []interface{}{"magnet:?xt=urn:btih:acecd763048a7fbfd32c2ef0e40cb1aad5b69233&dn=Twin+Peaks+S03+1080p+10bit+AMZN+WEBRip+x265+HEVC+6CH-MRN&tr=udp%3A%2F%2Ftracker.leechers-paradise.org%3A6969&tr=udp%3A%2F%2Fzer0day.ch%3A1337&tr=udp%3A%2F%2Fopen.demonii.com%3A1337&tr=udp%3A%2F%2Ftracker.coppersurfer.tk%3A6969&tr=udp%3A%2F%2Fexodus.desync.com%3A6969"}}
	for true {
		time.Sleep(999 * time.Second)
	}
}
