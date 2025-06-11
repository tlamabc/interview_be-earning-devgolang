// 📁 cmd/node/main.go
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"interview-be-earning/pkg/blockchain"
)

type BlockProposal struct {
	Block *blockchain.Block `json:"block"`
}

type Node struct {
	ID          string
	Role        string
	Port        string
	Peers       []string
	VoteCount   int
	PendingTxs  []*blockchain.Transaction
	ChainPath   string
	Mutex       sync.Mutex
	KeepRunning bool
}

func main() {
	peersEnv := os.Getenv("PEERS")
	peers := strings.Split(peersEnv, ",")
	node := &Node{
		ID:         os.Getenv("NODE_ID"),
		Role:       os.Getenv("ROLE"),
		Port:       os.Getenv("PORT"),
		Peers:      peers,
		ChainPath:  "/app/data/chain.json",
		KeepRunning: true,
	}

	if node.Role == "follower" {
		if !node.LocalChainExists() {
			log.Println("🔁 No local chain found — syncing from peers...")
			node.SyncFromPeers()
		} else {
			log.Println("✅ Local chain found — loading...")
			// TODO: LoadChainFromDisk
		}
	}

	http.HandleFunc("/ping", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "pong from %s", node.ID)
	})

	http.HandleFunc("/submit-tx", node.handleSubmitTx)
	http.HandleFunc("/propose-block", node.handleProposeBlock)
	http.HandleFunc("/receive-block", node.handleReceiveBlock)
	http.HandleFunc("/vote", node.handleVote)
	http.HandleFunc("/latest-height", node.handleLatestHeight)
	http.HandleFunc("/get-block", node.handleGetBlock)

	go func() {
		for node.KeepRunning {
			time.Sleep(10 * time.Second)
			log.Println("🔄 Node is alive...")
		}
	}()

	log.Println("✅ Starting node:", node.ID, "on port", node.Port)
	log.Fatal(http.ListenAndServe(":"+node.Port, nil))
}

func (n *Node) LocalChainExists() bool {
	_, err := os.Stat(n.ChainPath)
	return err == nil
}

func (n *Node) SyncFromPeers() {
	for _, peer := range n.Peers {
		resp, err := http.Get(peer + "/latest-height")
		if err != nil {
			log.Printf("❌ Cannot get height from %s\n", peer)
			continue
		}
		body, _ := ioutil.ReadAll(resp.Body)
		_ = resp.Body.Close()
		remoteHeight, _ := strconv.Atoi(string(body))

		log.Printf("ℹ️ Peer %s has height %d\n", peer, remoteHeight)

		for i := 1; i <= remoteHeight; i++ {
			url := fmt.Sprintf("%s/get-block?height=%d", peer, i)
			res, err := http.Get(url)
			if err != nil {
				log.Println("❌ Error getting block", err)
				continue
			}
			var block blockchain.Block
			json.NewDecoder(res.Body).Decode(&block)
			res.Body.Close()
			n.SaveBlock(block)
		}
		log.Println("✅ Chain synced from", peer)
		break
	}
}

func (n *Node) SaveBlock(block blockchain.Block) {
	data, _ := json.MarshalIndent(block, "", "  ")
	os.MkdirAll(filepath.Dir(n.ChainPath), 0755)
	ioutil.WriteFile(n.ChainPath, data, 0644)
}

func (n *Node) handleLatestHeight(w http.ResponseWriter, r *http.Request) {
	if n.LocalChainExists() {
		w.Write([]byte("1"))
	} else {
		w.Write([]byte("0"))
	}
}

func (n *Node) handleGetBlock(w http.ResponseWriter, r *http.Request) {
	height := r.URL.Query().Get("height")
	if height != "1" || !n.LocalChainExists() {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	data, _ := ioutil.ReadFile(n.ChainPath)
	w.Write(data)
}

func (n *Node) handleSubmitTx(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var tx blockchain.Transaction
	if err := json.NewDecoder(r.Body).Decode(&tx); err != nil {
		http.Error(w, "invalid tx", http.StatusBadRequest)
		return
	}

	n.Mutex.Lock()
	defer n.Mutex.Unlock()
	n.PendingTxs = append(n.PendingTxs, &tx)

	fmt.Fprintf(w, "✅ tx accepted by %s", n.ID)
}

func (n *Node) handleProposeBlock(w http.ResponseWriter, r *http.Request) {
	if n.Role != "leader" {
		http.Error(w, "not leader", http.StatusForbidden)
		return
	}
	go n.createBlockAndBroadcast()
	fmt.Fprintln(w, "🚀 Block proposed and sent to followers")
}

func (n *Node) createBlockAndBroadcast() {
	n.Mutex.Lock()
	txs := n.PendingTxs
	n.PendingTxs = nil
	n.Mutex.Unlock()

	if len(txs) == 0 {
		log.Println("❗ No pending transactions")
		return
	}

	block := blockchain.NewBlock(txs, "prev_hash_dummy")
	proposal := BlockProposal{Block: block}

	for _, peer := range n.Peers {
		go func(peerURL string) {
			data, _ := json.Marshal(proposal)
			resp, err := http.Post(peerURL+"/receive-block", "application/json", bytes.NewReader(data))
			if err != nil {
				log.Printf("❌ Failed to send block to %s: %v\n", peerURL, err)
				return
			}
			defer resp.Body.Close()
			log.Printf("📨 Sent block to %s\n", peerURL)
		}(peer)
	}

	n.SaveBlock(*block)
}

func (n *Node) handleReceiveBlock(w http.ResponseWriter, r *http.Request) {
	if n.Role != "follower" {
		http.Error(w, "only follower can receive block", http.StatusForbidden)
		return
	}

	var proposal BlockProposal
	if err := json.NewDecoder(r.Body).Decode(&proposal); err != nil {
		http.Error(w, "invalid block", http.StatusBadRequest)
		return
	}

	block := proposal.Block

	if block == nil || len(block.Transactions) == 0 {
		http.Error(w, "invalid block data", http.StatusBadRequest)
		return
	}

	log.Printf("📥 %s received block with %d txs\n", n.ID, len(block.Transactions))

	leaderURL := n.Peers[0]
	vote := map[string]string{
		"voter": n.ID,
		"vote":  "accept",
	}

	data, _ := json.Marshal(vote)
	resp, err := http.Post(leaderURL+"/vote", "application/json", bytes.NewReader(data))
	if err != nil {
		log.Printf("❌ Failed to send vote to %s: %v", leaderURL, err)
		return
	}
	defer resp.Body.Close()
	log.Printf("🗳️ Voted accept to leader from %s\n", n.ID)

	fmt.Fprintln(w, "✅ Block received and vote sent")
}

func (n *Node) handleVote(w http.ResponseWriter, r *http.Request) {
	if n.Role != "leader" {
		http.Error(w, "only leader accepts votes", http.StatusForbidden)
		return
	}

	var vote map[string]string
	if err := json.NewDecoder(r.Body).Decode(&vote); err != nil {
		http.Error(w, "invalid vote", http.StatusBadRequest)
		return
	}

	voter := vote["voter"]
	result := vote["vote"]
	log.Printf("🗳️ Received vote from %s: %s\n", voter, result)

	if result == "accept" {
		n.VoteCount++
	}

	if n.VoteCount >= 2 {
		log.Println("✅ Block committed by consensus 🎉")
		n.VoteCount = 0
	}
}
