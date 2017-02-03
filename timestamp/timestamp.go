// Package timestamp implements a simplified timestamp server.
// During one epoch it collects statements from
// clients, waits EpochDuration time and responds with a signature of the
// requested data.
package timestamp

import (
	"crypto/sha256"
	"encoding/binary"
	"sync"
	"time"

	"bytes"

	"github.com/dedis/paper_17_usenixsec_chainiac_cleanup/swupdate"
	"gopkg.in/dedis/onet.v1"
	"gopkg.in/dedis/onet.v1/log"
	"gopkg.in/dedis/onet.v1/network"
)

// ServiceName can be used to refer to the name of the timestamp service
const ServiceName = "Timestamp"

var timestampSID onet.ServiceID

var dummyVerfier = func(rootAndTimestamp []byte) bool {
	l := len(rootAndTimestamp)
	_, err := bytesToTimestamp(rootAndTimestamp[l-10 : l])
	if err != nil {
		log.Error("Got some invalid timestamp.")
	}
	return true
}

func init() {
	onet.RegisterNewService(ServiceName, newTimestampService)
	timestampSID = onet.ServiceFactory.ServiceID(ServiceName)
	network.RegisterMessage(&SignatureRequest{})
	network.RegisterMessage(&SignatureResponse{})
	network.RegisterMessage(&SetupRosterRequest{})
	network.RegisterMessage(&SetupRosterResponse{})
}

// Service handles client requests. It implements
type Service struct {
	*onet.ServiceProcessor
	// Epoch is is the time that needs to pass until
	// the timestamp service attempts to collectively sign the batches
	// of statements collected. Reasonable choices would be from 10 seconds
	// upto some hours.
	EpochDuration time.Duration

	// mainly for testing purposes:
	maxIterations int

	// config path for service:
	path string
	// collected data for one epoch:
	requests requestPool
	roster   *onet.Roster
	// easy to change from one signer (cosi) to another (mock/BFTcosi):
	signMsg func(m []byte) []byte
}

// NewProtocol is called on all nodes of a Tree (except the root, since it is
// the one starting the protocol) so it's the Service that will be called to
// generate the PI on all others node.
func (s *Service) NewProtocol(tn *onet.TreeNodeInstance, conf *onet.GenericConfig) (onet.ProtocolInstance, error) {
	log.Lvl2("Timestamp Service received New Protocol event")
	pi, err := swupdate.NewCoSiUpdate(tn, dummyVerfier)
	return pi, err
}

// SignatureRequest will be requested by clients.
type SignatureRequest struct {
	// Message should be a hashed nonce for the timestamp server.
	Message []byte
	// Different requests will be signed by the same roster
	// Hence, it doesn't make sense for every client to send his Roster
	// Roster  *onet.Roster
}

// SetupRosterRequest can be send by a client to initialize the service.
// It defines the roster that will be used, the epoch duration and (optionally)
// the number of iterations the service will run.
type SetupRosterRequest struct {
	Roster        *onet.Roster
	EpochDuration time.Duration
	MaxIterations int
}

// SetupRosterResponse returns the ID of the roster if the init. was successful.
type SetupRosterResponse struct {
	ID *onet.RosterID
}

// SignatureResponse is what the Cosi service will reply to clients.
type SignatureResponse struct {
	// The time in seconds when the request was started:
	Timestamp int64
	// The tree root that was signed:
	Root HashID
	// Proof is an Inclusion proof for the data the client requested:
	Proof Proof
	// Collective signature on Timestamp||hash(treeroot):
	Signature []byte

	// TODO should we return the roster used to sign this message?
}

// SignatureRequest treats external request to this service.
func (s *Service) SignatureRequest(si *network.ServerIdentity, req *SignatureRequest) (network.Message, error) {

	// on every request:
	// 1) If has the length of hashed nonce, add it to the local buffer of
	//    of the service:
	respC := make(chan *SignatureResponse)
	s.requests.Add(req.Message, respC)
	// 2) At epoch time: create the merkle tree
	// see runLoop
	// 3) run *one* cosi round on treeroot||timestamp
	// see runLoop
	// 4) return to each client: above signature, (inclusion) Proof, timestamp
	// see runLoop

	// wait on my signature:
	log.Lvl2("Waiting on epoch end.")
	resp := <-respC
	return resp, nil
}

// SetupCoSiRoster handles `SetupRosterRequest`s requests
// XXX later we'll give it an ID instead of the actual roster?
func (s *Service) SetupCoSiRoster(si *network.ServerIdentity, setup *SetupRosterRequest) (network.Message, error) {
	if s.roster == nil && s.EpochDuration == 0 {
		s.roster = setup.Roster
		s.EpochDuration = setup.EpochDuration
		s.maxIterations = setup.MaxIterations
		go s.runLoop()
		log.Lvl1("Started main loop with epoch duration:", s.EpochDuration)
	} else {
		log.Warnf("Timestamper already initialized and received init. request!"+
			" Running with epoch duration %v (max. %v iterations) and with roster %v",
			s.EpochDuration, s.maxIterations)
	}
	return &SetupRosterResponse{ID: &s.roster.ID}, nil

}

func (s *Service) cosiSign(msg []byte) []byte {
	sdaTree := s.roster.GenerateBinaryTree()

	tni := s.NewTreeNodeInstance(sdaTree, sdaTree.Root, swupdate.ProtocolName)
	pi, err := swupdate.NewCoSiUpdate(tni, dummyVerfier)
	if err != nil {
		panic("Couldn't make new protocol: " + err.Error())
	}
	s.RegisterProtocolInstance(pi)

	pi.SigningMessage(msg)
	// Take the raw message (already expecting a hash for the timestamp
	// service)
	response := make(chan []byte)
	pi.RegisterSignatureHook(func(sig []byte) {
		response <- sig
	})
	go pi.Dispatch()
	go pi.Start()
	res := <-response
	log.Lvl2("Recieved cosi response")
	return res

}

// main loop
func (s *Service) runLoop() {
	c := time.Tick(s.EpochDuration)
	counter := 0
	log.Lvl4("Starting main loop:")
	for now := range c /*TODO interrupt the main loop must be possible*/ {

		counter++
		if counter > s.maxIterations && s.maxIterations > 0 {
			log.Info("Max epoch reached... Quitting main loop.")
			break
		}
		// only sign something if there was some data/requests:
		data, channels := s.requests.GetData()
		s.requests.reset()
		numRequests := len(data)
		if numRequests > 0 {
			log.Lvl2("Signin tree root with timestampt:", now, "got", numRequests, "requests")

			// create merkle tree and message to be signed:
			root, proofs := ProofTree(sha256.New, data)
			msg := RecreateSignedMsg(root, now.Unix())

			signature := s.signMsg(msg)
			log.Lvlf2("%s: Signed a message.\n", time.Now().Format("Mon Jan 2 15:04:05 -0700 MST 2006"))
			// Give (individual) response to anyone waiting:
			for i, respC := range channels {
				respC <- &SignatureResponse{
					Timestamp: now.Unix(),
					Proof:     proofs[i],
					Root:      root,
					// Collective signature on Timestamp||hash(treeroot)
					Signature: signature,
				}
			}
		} else {
			log.Lvl3("No requests at epoch:", time.Now().Format("Mon Jan 2 15:04:05 -0700 MST 2006"))
		}
	}

}

func timestampToBytes(t int64) []byte {
	timeBuf := make([]byte, binary.MaxVarintLen64)
	binary.PutVarint(timeBuf, t)
	return timeBuf
}

func bytesToTimestamp(b []byte) (int64, error) {
	t, err := binary.ReadVarint(bytes.NewReader(b))
	if err != nil {
		return t, err
	}
	return t, nil
}

func newTimestampService(c *onet.Context, path string) onet.Service {
	log.Lvl4("New Service created!")
	s := &Service{
		ServiceProcessor: onet.NewServiceProcessor(c),
		path:             path,
		requests:         requestPool{},
		// EpochDuration must be initialized by sending a setup req.
	}
	s.signMsg = s.cosiSign
	err := s.RegisterHandler(s.SignatureRequest)
	if err != nil {
		log.ErrFatal(err, "Couldn't register message:")
	}
	err = s.RegisterHandler(s.SetupCoSiRoster)
	if err != nil {
		log.ErrFatal(err, "Couldn't register message:")
	}

	// start main loop:
	// XXX will be triggered by init. message instead, makes the simulation
	// easier but has the big downside, that the client who sends the initial
	//
	//go s.runLoop()

	return s
}

type tree struct {
	proofs []Proof
	root   HashID
}

type requestPool struct {
	sync.Mutex
	requestData      []HashID
	responseChannels []chan *SignatureResponse
}

func (rb *requestPool) reset() {
	rb.Lock()
	defer rb.Unlock()
	rb.requestData = nil
	// XXX do we need to close each channel separately?
	rb.responseChannels = nil
}

func (rb *requestPool) Add(data []byte, responseChan chan *SignatureResponse) {
	rb.Lock()
	defer rb.Unlock()
	rb.requestData = append(rb.requestData, data)
	log.Lvl5("Added request", len(rb.requestData), string(data))
	rb.responseChannels = append(rb.responseChannels, responseChan)
}

func (rb *requestPool) GetData() ([]HashID, []chan *SignatureResponse) {
	rb.Lock()
	defer rb.Unlock()
	return rb.requestData, rb.responseChannels
}

// RecreateSignedMsg is a helper that can be used by the client to recreate the
// message signed by the timestamp service (which is treeroot||timestamp)
func RecreateSignedMsg(treeroot []byte, timestamp int64) []byte {
	timeB := timestampToBytes(timestamp)
	m := make([]byte, len(treeroot)+len(timeB))
	m = append(m, treeroot...)
	m = append(m, timeB...)
	return m
}
