package rendezvous

import (
	"encoding/json"
	"fmt"
	"io"
	"sync"
	"time"

	pb "github.com/berty/go-libp2p-rendezvous/pb"
	"github.com/libp2p/go-libp2p/core/host"
	inet "github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/core/protocol"
	"google.golang.org/protobuf/proto"
)

const (
	ServiceType  = "inmem"
	ServiceProto = protocol.ID("/rendezvous/sync/inmem/1.0.0")
)

type PubSub struct {
	mu     sync.RWMutex
	host   host.Host
	topics map[string]*PubSubSubscribers
}

type PubSubSubscribers struct {
	mu               sync.RWMutex
	subscribers      map[peer.ID]io.Writer
	lastAnnouncement *pb.RegistrationRecord
}

type PubSubSubscriptionDetails struct {
	PeerID      string
	ChannelName string
}

func NewSyncInMemProvider(host host.Host) (*PubSub, error) {
	ps := &PubSub{
		host:   host,
		topics: map[string]*PubSubSubscribers{},
	}

	ps.Listen()

	return ps, nil
}

func (ps *PubSub) Subscribe(ns string) (syncDetails string, err error) {
	details, err := json.Marshal(&PubSubSubscriptionDetails{
		PeerID:      ps.host.ID().String(),
		ChannelName: ns,
	})
	if err != nil {
		return "", fmt.Errorf("unable to marshal subscription details: %w", err)
	}

	return string(details), nil
}

func (ps *PubSub) GetServiceType() string {
	return ServiceType
}

func (ps *PubSub) getOrCreateTopic(ns string) *PubSubSubscribers {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if subscribers, ok := ps.topics[ns]; ok {
		return subscribers
	}

	ps.topics[ns] = &PubSubSubscribers{
		subscribers:      map[peer.ID]io.Writer{},
		lastAnnouncement: nil,
	}
	return ps.topics[ns]
}

func (ps *PubSub) Register(pid peer.ID, ns string, addrs [][]byte, ttlAsSeconds int, counter uint64) {
	topic := ps.getOrCreateTopic(ns)
	data := &pb.RegistrationRecord{
		Id:    pid.String(),
		Addrs: addrs,
		Ns:    ns,
		Ttl:   time.Now().Add(time.Duration(ttlAsSeconds) * time.Second).UnixMilli(),
	}
	dataBytes, err := proto.Marshal(data)
	if err != nil {
		log.Errorf("unable to marshal registration record: %s", err.Error())
		return
	}

	topic.mu.Lock()
	topic.lastAnnouncement = data
	toNotify := topic.subscribers
	for _, stream := range toNotify {
		if _, err := stream.Write(dataBytes); err != nil {
			log.Errorf("unable to notify rendezvous data update: %s", err.Error())
		}
	}
	topic.mu.Unlock()
}

func (ps *PubSub) Unregister(p peer.ID, ns string) {
	// TODO: unsupported
}

func (ps *PubSub) Listen() {
	ps.host.SetStreamHandler(ServiceProto, ps.handleStream)
}

func (ps *PubSub) handleStream(s inet.Stream) {
	defer s.Reset()

	subscribedTopics := map[string]struct{}{}

	for {
		var req pb.Message
		buffer := make([]byte, inet.MessageSizeMax)

		defer func() {
			for ns := range subscribedTopics {
				topic := ps.getOrCreateTopic(ns)
				topic.mu.Lock()
				delete(topic.subscribers, s.Conn().RemotePeer())
				topic.mu.Unlock()
			}
		}()

		n, err := s.Read(buffer)
		if err != nil && err != io.EOF {
			log.Errorf("unable to read from stream: %s", err.Error())
			return
		}

		err = proto.Unmarshal(buffer[:n], &req)
		if err != nil {
			log.Errorf("error unmarshalling request: %s", err.Error())
			return
		}

		if req.Type != pb.Message_DISCOVER_SUBSCRIBE {
			continue
		}

		topic := ps.getOrCreateTopic(req.DiscoverSubscribe.Ns)
		topic.mu.Lock()
		if _, ok := topic.subscribers[s.Conn().RemotePeer()]; ok {
			topic.mu.Unlock()
			continue
		}

		topic.subscribers[s.Conn().RemotePeer()] = s
		subscribedTopics[req.DiscoverSubscribe.Ns] = struct{}{}
		lastAnnouncement := topic.lastAnnouncement
		if lastAnnouncement != nil {
			msgBytes, err := proto.Marshal(lastAnnouncement)
			if err != nil {
				log.Errorf("error marshalling response: %s", err.Error())
				continue
			}
			if _, err := s.Write(msgBytes); err != nil {
				log.Errorf("unable to write announcement: %s", err.Error())
			}
		}
		topic.mu.Unlock()
	}
}

var (
	_ RendezvousSync             = (*PubSub)(nil)
	_ RendezvousSyncSubscribable = (*PubSub)(nil)
)
