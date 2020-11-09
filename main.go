package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"time"

	"github.com/ipfs/go-cid"
	config "github.com/ipfs/go-ipfs-config"
	files "github.com/ipfs/go-ipfs-files"
	libp2p "github.com/ipfs/go-ipfs/core/node/libp2p"
	icore "github.com/ipfs/interface-go-ipfs-core"
	lp2p "github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/p2p/discovery"
	ma "github.com/multiformats/go-multiaddr"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"

	"github.com/ipfs/go-ipfs/core"
	"github.com/ipfs/go-ipfs/core/coreapi"
	"github.com/ipfs/go-ipfs/plugin/loader" // This package is needed so that all the preloaded plugins are loaded automatically
	"github.com/ipfs/go-ipfs/repo/fsrepo"
	"github.com/libp2p/go-libp2p-core/host"
	"github.com/libp2p/go-libp2p-core/peer"
)

/// ------ Setting up the IPFS Repo

func setupPlugins(externalPluginsPath string) error {
	// Load any external plugins if available on externalPluginsPath
	plugins, err := loader.NewPluginLoader(filepath.Join(externalPluginsPath, "plugins"))
	if err != nil {
		return fmt.Errorf("error loading plugins: %s", err)
	}

	// Load preloaded and external plugins
	if err := plugins.Initialize(); err != nil {
		return fmt.Errorf("error initializing plugins: %s", err)
	}

	if err := plugins.Inject(); err != nil {
		return fmt.Errorf("error initializing plugins: %s", err)
	}

	return nil
}

func createTempRepo(ctx context.Context) (string, error) {
	repoPath, err := ioutil.TempDir("", "ipfs-shell")
	if err != nil {
		return "", fmt.Errorf("failed to get temp dir: %s", err)
	}

	// Create a config with default options and a 2048 bit key
	cfg, err := config.Init(ioutil.Discard, 2048)
	if err != nil {
		return "", err
	}

	// Set Swarm listening to a random address
	randAddr, _ := ma.NewMultiaddr("/ip4/0.0.0.0/tcp/0")
	cfg.Addresses.Swarm = []string{randAddr.String()}

	// Only discover local nodes
	cfg.Bootstrap = []string{}

	// Create the repo with the config
	err = fsrepo.Init(repoPath, cfg)
	if err != nil {
		return "", fmt.Errorf("failed to init ephemeral node: %s", err)
	}

	return repoPath, nil
}

/// ------ Spawning the node

// Creates an IPFS node and returns its coreAPI
func createNode(ctx context.Context, repoPath string) (*core.IpfsNode, icore.CoreAPI, error) {
	// Open the repo
	repo, err := fsrepo.Open(repoPath)
	if err != nil {
		return nil, nil, err
	}

	// Construct the node

	nodeOptions := &core.BuildCfg{
		Online:  true,
		Routing: libp2p.DHTOption, // This option sets the node to be a full DHT node (both fetching and storing DHT Records)
		// Routing: libp2p.DHTClientOption, // This option sets the node to be a client DHT node (only fetching records)
		Repo: repo,
		ExtraOpts: map[string]bool{
			"pubsub": true,
		},
	}

	node, err := core.NewNode(ctx, nodeOptions)
	if err != nil {
		return nil, nil, err
	}

	// Attach the Core API to the constructed node
	api, err := coreapi.NewCoreAPI(node)
	return node, api, err
}

// Spawns a node to be used just for this run (i.e. creates a tmp repo)
func spawnEphemeral(ctx context.Context) (*core.IpfsNode, icore.CoreAPI, error) {
	if err := setupPlugins(""); err != nil {
		return nil, nil, err
	}

	// Create a Temporary Repo
	repoPath, err := createTempRepo(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create temp repo: %s", err)
	}

	// Spawning an ephemeral IPFS node
	return createNode(ctx, repoPath)
}

//

func getUnixfsFile(path string) (files.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	st, err := file.Stat()
	if err != nil {
		return nil, err
	}

	f, err := files.NewReaderPathFile(path, file, st)
	if err != nil {
		return nil, err
	}

	return f, nil
}

func getUnixfsNode(path string) (files.Node, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	f, err := files.NewSerialFile(path, false, st)
	if err != nil {
		return nil, err
	}

	return f, nil
}

// ------- Gossip sub --------------

const PinsTopic = "gossip-pins"

type GossipMessage struct {
	PayloadCID cid.Cid
	SenderID   peer.ID
}

// GossipChannel represents a subscription to a topic
type GossipChannel struct {
	Messages chan *GossipMessage

	ctx   context.Context
	ps    *pubsub.PubSub
	topic *pubsub.Topic
	sub   *pubsub.Subscription

	self peer.ID
}

type Gossip struct {
	Pins *GossipChannel

	ps *pubsub.PubSub
}

func NewGossip(ctx context.Context, h host.Host) (*Gossip, error) {
	ps, err := pubsub.NewGossipSub(ctx, h)
	if err != nil {
		return nil, err
	}
	pins, err := NewGossipChannel(ctx, PinsTopic, ps, h.ID())
	if err != nil {
		return nil, err
	}
	g := &Gossip{
		Pins: pins,
		ps:   ps,
	}
	return g, nil
}

func NewGossipChannel(ctx context.Context, topicName string, ps *pubsub.PubSub, selfID peer.ID) (*GossipChannel, error) {
	topic, err := ps.Join(topicName)
	if err != nil {
		return nil, err
	}

	sub, err := topic.Subscribe()
	if err != nil {
		return nil, err
	}

	c := &GossipChannel{
		ctx:      ctx,
		ps:       ps,
		sub:      sub,
		self:     selfID,
		topic:    topic,
		Messages: make(chan *GossipMessage),
	}
	go c.readLoop()
	return c, nil
}

// readLoop pulls messages from the pubsub topic and pushes them onto the Messages channel.
func (c *GossipChannel) readLoop() {
	for {
		msg, err := c.sub.Next(c.ctx)
		fmt.Println("Got msg")
		if err != nil {
			close(c.Messages)
			return
		}
		// only forward messages delivered by others
		if msg.ReceivedFrom == c.self {
			continue
		}
		m := new(GossipMessage)
		err = json.Unmarshal(msg.Data, m)
		if err != nil {
			continue
		}
		fmt.Println("Got new Gossip message")
		// send valid messages onto the Messages channel
		c.Messages <- m
	}
}

// Publish sends new content to the pubsub topic
func (c *GossipChannel) Publish(payload cid.Cid) error {
	m := GossipMessage{
		PayloadCID: payload,
		SenderID:   c.self,
	}
	msgBytes, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return c.topic.Publish(c.ctx, msgBytes)
}

func (gc *GossipChannel) PeerNum() int {
	peers := gc.ps.ListPeers(PinsTopic)
	return len(peers)
}

/// ------- Libp2p host ------------

func createHost(ctx context.Context) (host.Host, error) {
	// create a new libp2p Host that listens on a random TCP port
	h, err := lp2p.New(ctx, lp2p.ListenAddrStrings("/ip4/0.0.0.0/tcp/0"))
	if err != nil {
		return h, err
	}

	// setup local mDNS discovery
	err = setupDiscovery(ctx, h)

	return h, err
}

/// -------

func main() {

	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Spawn a node using a temporary path, creating a temporary repo for the run
	fmt.Println("Spawning node on a temporary repo")
	ipfs, api, err := spawnEphemeral(ctx)
	if err != nil {
		panic(fmt.Errorf("failed to spawn ephemeral node: %s", err))
	}

	fmt.Println("IPFS node is running")

	// ---------- Uncomment below to provide a standalone libp2p host --------------
	// Create a custom libp2p host to use for gossip sub
	// h, err := createHost(ctx)
	// if err != nil {
	// 	log.Fatal().Err(err).Msg("Couldn't setup libp2p host")
	// }

	g, err := NewGossip(ctx, ipfs.PeerHost)

	go func() {
		for {
			time.Sleep(5 * time.Second)
			c, err := cid.Decode("QmUaoioqU7bxezBQZkUcgcSyokatMY71sxsALxQmRRrHrj")

			if err != nil {
				continue
			}
			if err := g.Pins.Publish(c); err != nil {
				log.Error().Err(err).Msg("Couldn't publish cid to gossip")
			}

			ipfsPeers, _ := api.Swarm().Peers(ctx)

			log.Info().Str("cid", c.String()).
				Int("peers", g.Pins.PeerNum()).
				Int("ipfsPeers", len(ipfsPeers)).
				Msg("Publishing")
		}
	}()

	select {}
}

// DiscoveryInterval is how often we re-publish our mDNS records.
const DiscoveryInterval = time.Hour

// DiscoveryServiceTag is used in our mDNS advertisements to discover other chat peers.
const DiscoveryServiceTag = "pubsub-chat-example"

// printErr is like fmt.Printf, but writes to stderr.
func printErr(m string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, m, args...)
}

// defaultNick generates a nickname based on the $USER environment variable and
// the last 8 chars of a peer ID.
func defaultNick(p peer.ID) string {
	return fmt.Sprintf("%s-%s", os.Getenv("USER"), shortID(p))
}

// shortID returns the last 8 chars of a base58-encoded peer id.
func shortID(p peer.ID) string {
	pretty := p.Pretty()
	return pretty[len(pretty)-8:]
}

// discoveryNotifee gets notified when we find a new peer via mDNS discovery
type discoveryNotifee struct {
	h host.Host
}

// HandlePeerFound connects to peers discovered via mDNS. Once they're connected,
// the PubSub system will automatically start interacting with them if they also
// support PubSub.
func (n *discoveryNotifee) HandlePeerFound(pi peer.AddrInfo) {
	fmt.Printf("discovered new peer %s\n", pi.ID.Pretty())
	err := n.h.Connect(context.Background(), pi)
	if err != nil {
		fmt.Printf("error connecting to peer %s: %s\n", pi.ID.Pretty(), err)
	}
}

// setupDiscovery creates an mDNS discovery service and attaches it to the libp2p Host.
// This lets us automatically discover peers on the same LAN and connect to them.
func setupDiscovery(ctx context.Context, h host.Host) error {
	// setup mDNS discovery to find local peers
	disc, err := discovery.NewMdnsService(ctx, h, DiscoveryInterval, DiscoveryServiceTag)
	if err != nil {
		return err
	}

	n := discoveryNotifee{h: h}
	disc.RegisterNotifee(&n)
	return nil
}
