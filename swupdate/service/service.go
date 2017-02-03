package swupdate

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io/ioutil"
	"os"
	"sort"

	"sync"

	"time"

	"os/exec"

	"strings"

	"strconv"

	"github.com/dedis/paper_17_usenixsec_chainiac/manage"
	"github.com/dedis/paper_17_usenixsec_chainiac/skipchain"
	"github.com/dedis/paper_17_usenixsec_chainiac/swupdate/protocol"
	"github.com/dedis/paper_17_usenixsec_chainiac/timestamp"
	"github.com/satori/go.uuid"
	"gopkg.in/dedis/onet.v1"
	"gopkg.in/dedis/onet.v1/log"
	"gopkg.in/dedis/onet.v1/network"
	"gopkg.in/dedis/onet.v1/simul/monitor"
)

// This file contains all the code to run a CoSi service. It is used to reply to
// client request for signing something using CoSi.
// As a prototype, it just signs and returns. It would be very easy to write an
// updated version that chains all signatures for example.

// ServiceName is the name to refer to the CoSi service
const ServiceName = "Swupdate"

var swupdateService onet.ServiceID
var verifierID = skipchain.VerifierID(uuid.NewV5(uuid.NamespaceURL, ServiceName))

func init() {
	onet.RegisterNewService(ServiceName, newSwupdate)
	swupdateService = onet.ServiceFactory.ServiceID(ServiceName)
	network.RegisterMessage(&storage{})
	skipchain.VerificationRegistration(verifierID, verifierFunc)
}

// Swupdate allows decentralized software-update-signing and verification.
type Service struct {
	*onet.ServiceProcessor
	path      string
	skipchain *skipchain.Client
	Storage   *storage
	tsChannel chan string
	sync.Mutex
	// how much time a timestamp is considered valid
	ReasonableTime time.Duration
}

type storage struct {
	// A timestamp over all skipchains where all skipblocks are
	// included in a Merkle-tree.
	Timestamp         *Timestamp
	SwupChainsGenesis map[string]*SwupChain
	SwupChains        map[string]*SwupChain
	Root              *skipchain.SkipBlock
	TSInterval        time.Duration
}

// CreateProject is the starting point of the software-update and will
// - initialize the skipchains
// - return an id of how this project can be referred to
func (cs *Service) CreatePackage(cp *CreatePackage) (network.Message, onet.ClientError) {
	policy := cp.Release.Policy
	log.Lvlf3("%s Creating package %s version %s", cs,
		policy.Name, policy.Version)
	sc := &SwupChain{
		Release: cp.Release,
		Root:    cs.Storage.Root,
	}
	if cs.Storage.Root == nil {
		log.Lvl3("Creating Root-skipchain")
		var err error
		cs.Storage.Root, err = cs.skipchain.CreateRoster(cp.Roster, cp.Base, cp.Height,
			skipchain.VerifyNone, nil)
		if err != nil {
			return nil, onet.NewClientError(err)
		}
		sc.Root = cs.Storage.Root
	}
	log.Lvl3("Creating Data-skipchain")
	var err error
	sc.Root, sc.Data, err = cs.skipchain.CreateData(sc.Root, cp.Base, cp.Height,
		verifierID, cp.Release)
	if err != nil {
		return nil, onet.NewClientError(err)
	}
	cs.Storage.SwupChainsGenesis[policy.Name] = sc
	if err := cs.startPropagate(policy.Name, sc); err != nil {
		return nil, onet.NewClientError(err)
	}
	cs.timestamp(time.Now())

	return &CreatePackageRet{sc}, nil
}

// SignatureRequest treats external request to this service.
func (cs *Service) UpdatePackage(up *UpdatePackage) (network.Message, onet.ClientError) {
	addBlock := monitor.NewTimeMeasure("add_block")
	defer addBlock.Record()
	sc := &SwupChain{
		Release: up.Release,
		Root:    up.SwupChain.Root,
	}
	rel := up.Release
	log.Lvl3("Creating Data-skipchain")
	var err error
	psbrep, err := cs.skipchain.ProposeData(up.SwupChain.Root,
		up.SwupChain.Data, rel)
	if err != nil {
		return nil, onet.NewClientError(err)
	}
	sc.Data = psbrep.Latest

	if err := cs.startPropagate(rel.Policy.Name, sc); err != nil {
		return nil, onet.NewClientError(err)
	}
	cs.timestamp(time.Now())
	return &UpdatePackageRet{sc}, nil
}

// PropagateSkipBlock will save a new SkipBlock
func (cs *Service) PropagateSkipBlock(msg network.Message) {
	sc, ok := msg.(*SwupChain)
	if !ok {
		log.Error("Couldn't convert to SkipBlock")
		return
	}
	pkg := sc.Release.Policy.Name
	log.Lvl2("saving swupchain for", pkg)
	// TODO: verification
	//if err := sb.VerifySignatures(); err != nil {
	//	log.Error(err)
	//	return
	//}
	if _, exists := cs.Storage.SwupChainsGenesis[pkg]; !exists {
		cs.Storage.SwupChainsGenesis[pkg] = sc
	}
	cs.Storage.SwupChains[pkg] = sc
}

// Propagate the new block
func (cs *Service) startPropagate(pkg string, sc *SwupChain) error {
	roster := cs.Storage.Root.Roster
	log.Lvl2("Propagating package", pkg, "to", roster.List)
	replies, err := manage.PropagateStartAndWait(cs.Context, roster, sc, 120000, cs.PropagateSkipBlock)
	if err != nil {
		return err
	}
	if replies != len(roster.List) {
		log.Warn("Did only get", replies, "out of", len(roster.List))
	}
	return nil
}

// PackageSC searches for the skipchain containing the package. If it finds a
// skipchain, it returns the first and the last block. If no skipchain for
// that package is found, it returns nil for the first and last block.
func (cs *Service) PackageSC(psc *PackageSC) (network.Message, onet.ClientError) {
	sc, ok := cs.Storage.SwupChains[psc.PackageName]
	if !ok {
		return nil, onet.NewClientErrorCode(4200, "Does not exist.")
	}
	lbRet, err := cs.LatestBlock(&LatestBlock{sc.Data.Hash})
	if err != nil {
		return nil, onet.NewClientError(err)
	}
	update := lbRet.(*LatestBlockRet).Update
	return &PackageSCRet{
		First: cs.Storage.SwupChainsGenesis[psc.PackageName].Data,
		Last:  update[len(update)-1],
	}, nil
}

// LatestBlock returns the hash of the latest block together with a timestamp
// signed by all nodes of the swupdate-skipchain responsible for that package
func (cs *Service) LatestBlock(lb *LatestBlock) (network.Message, onet.ClientError) {
	gucRet, err := cs.skipchain.GetUpdateChain(cs.Storage.Root, lb.LastKnownSB)
	if err != nil {
		return nil, onet.NewClientError(err)
	}
	if cs.Storage.Timestamp == nil {
		return nil, onet.NewClientErrorCode(4200, "Timestamp-service missing!")
	}
	return &LatestBlockRet{cs.Storage.Timestamp, gucRet.Update}, nil
}

func (cs *Service) LatestBlocks(lbs *LatestBlocks) (network.Message, onet.ClientError) {
	var updates []*skipchain.SkipBlock
	var lengths []int64
	var t *Timestamp
	for _, id := range lbs.LastKnownSBs {
		//log.Print(lbs.LastKnownSBs, id)
		b, err := cs.LatestBlock(&LatestBlock{id})
		if err != nil {
			return nil, onet.NewClientError(err)
		}
		lb := b.(*LatestBlockRet)
		if len(lb.Update) > 1 {
			updates = append(updates, lb.Update...)
			lengths = append(lengths, int64(len(lb.Update)))
			if t == nil {
				t = lb.Timestamp
			}
			//log.Print(i, updates, lb.Update)
		}
	}
	return &LatestBlocksRetInternal{t, updates, lengths}, nil
}

// NewProtocol will instantiate a new cosi-timestamp protocol.
func (cs *Service) NewProtocol(tn *onet.TreeNodeInstance, conf *onet.GenericConfig) (onet.ProtocolInstance, error) {
	var pi onet.ProtocolInstance
	var err error
	switch tn.ProtocolName() {
	case "Propagate":
		log.Lvl2("SWUpdate Service received New Protocol PROPAGATE  event")
		pi, err = manage.NewPropagateProtocol(tn)
		if err != nil {
			return nil, err
		}
		pi.(*manage.Propagate).RegisterOnData(cs.PropagateSkipBlock)
	default:
		log.Lvl2("SWUpdate Service received New Protocol COSI event")
		pi, err = swupdate.NewCoSiUpdate(tn, cs.cosiVerify)
		if err != nil {
			return nil, err
		}
	}
	return pi, err
}

// timestamper waits for n minutes before asking all nodes to timestamp
// on the latest version of all skipblocks.
// This function only returns when "close" is sent through the
// tsChannel.
func (cs *Service) timestamper() {
	for true {
		select {
		case msg := <-cs.tsChannel:
			switch msg {
			case "close":
				return
			case "update":
			default:
				log.Error("Don't know message", msg)
			}
		case <-time.After(cs.Storage.TSInterval):
			log.Lvl2("Interval is over - timestamping")
		}
		// Start timestamping
	}
}

// verifierFunc will return whether the block is valid.
func verifierFunc(msg, data []byte) bool {
	_, sbBuf, err := network.Unmarshal(data)
	sb, ok := sbBuf.(*skipchain.SkipBlock)
	if err != nil || !ok {
		log.Error(err, ok)
		return false
	}
	_, relBuf, err := network.Unmarshal(sb.Data)
	release, ok := relBuf.(*Release)
	if err != nil || !ok {
		log.Error(err, ok)
		return false
	}
	policy := release.Policy
	policyBin, err := network.Marshal(policy)
	if err != nil {
		log.Error(err)
		return false
	}
	ver := monitor.NewTimeMeasure("verification")
	//log.Printf("Verifying release %s/%s", policy.Name, policy.Version)
	for i, s := range release.Signatures {
		err := NewPGPPublic(policy.Keys[i]).Verify(
			policyBin, s)
		if err != nil {
			log.Lvl2("Wrong signature")
			return false
		}
	}
	ver.Record()
	wall := 2.0
	user := 0.0
	system := 0.0
	if release.VerifyBuild {
		// Verify the reproducible build
		log.LLvl1("Starting to build", policy.Name, policy.Version)
		cmd := exec.Command("./crawler.py",
			"cli", policy.Name)
		cmd.Stderr = os.Stderr
		resultB, err := cmd.Output()
		result := string(resultB)
		if err != nil {
			wd, _ := os.Getwd()
			log.Error("While creating reproducible build:", err, result, wd)
		} else {
			log.Lvl2("Build-output is", result)
			resArray := strings.Split(result, "\n")
			res := resArray[len(resArray)-2]
			log.Lvl2("Last line is", res)
			if strings.Index(res, "Success") >= 0 {
				times := strings.Split(res, " ")
				wall, err = strconv.ParseFloat(times[1], 64)
				if err != nil {
					log.Error(err)
				}
				user, err = strconv.ParseFloat(times[2], 64)
				if err != nil {
					log.Error(err)
				}
				system, err = strconv.ParseFloat(times[3], 64)
				if err != nil {
					log.Error(err)
				}
			} else {
				wall = 0.0
			}
		}
		if wall+user+system > 0.0 {
			log.LLvl2("Congrats, verified", policy.Name, policy.Version, "in", wall, user, system)
		}
	}
	monitor.RecordSingleMeasure("build_wall", user)
	monitor.RecordSingleMeasure("build_user", user)
	monitor.RecordSingleMeasure("build_system", system)
	monitor.RecordSingleMeasure("build_cpu", user+system)
	return true
}

// saves the actual identity
func (s *Service) save() {
	log.Lvl3("Saving service")
	b, err := network.Marshal(s.Storage)
	if err != nil {
		log.Error("Couldn't marshal service:", err)
	} else {
		err = ioutil.WriteFile(s.path+"/swupdate.bin", b, 0660)
		if err != nil {
			log.Error("Couldn't save file:", err)
		}
	}
}

// Tries to load the configuration and updates if a configuration
// is found, else it returns an error.
func (s *Service) tryLoad() error {
	configFile := s.path + "/swupdate.bin"
	b, err := ioutil.ReadFile(configFile)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("Error while reading %s: %s", configFile, err)
	}
	if len(b) > 0 {
		_, msg, err := network.Unmarshal(b)
		if err != nil {
			return fmt.Errorf("Couldn't unmarshal: %s", err)
		}
		// Only overwrite storage if we have a content,
		// else keep the pre-defined storage-map.
		if len(msg.(*storage).SwupChains) > 0 {
			log.Lvl3("Successfully loaded")
			s.Storage = msg.(*storage)
		}
	}
	return nil
}

func (s *Service) TimestampProofs(req *TimestampRequests) (network.Message, onet.ClientError) {
	// get the indexes in the list of packages
	keys := s.getOrderedPackageNames()
	var indexes []int
	for _, name := range req.Names {
		i := sort.SearchStrings(keys, name)
		if i < 0 || keys[i] != name {
			log.Error("No package at this name")
			return nil, onet.NewClientErrorCode(4200, "No package at this name")
		}
		indexes = append(indexes, i)
	}

	// then take all the proofs from these indexes
	p := s.Storage.Timestamp.Proofs
	var proofs = make(map[string]timestamp.Proof)
	for _, i := range indexes {
		name := keys[i]
		proofs[name] = p[i]
	}
	return &TimestampRets{proofs}, nil
}

// TimestampProof takes a package name and returns the latest proof of inclusion
// of the latest block's Hash in the timestamp Merkle Tree.
func (s *Service) TimestampProof(req *TimestampRequest) (network.Message, onet.ClientError) {
	s.Lock()
	defer s.Unlock()
	// get the index in the list of packages
	keys := s.getOrderedPackageNames()
	var idx int
	var found bool
	for i, s := range keys {
		if s == req.Name {
			idx = i
			found = true
			break
		}
	}
	if !found {
		log.Error("No package at this name")
		return nil, onet.NewClientErrorCode(4200, "No package at this name")
	}
	// then get the proof
	p := s.Storage.Timestamp.Proofs
	if len(p) < idx {
		log.Print("Before: something's wrong with this service")
		panic("something's wrong with this service")
	}
	return &TimestampRet{p[idx]}, nil
}

func (s *Service) getOrderedPackageNames() []string {
	keys := make([]string, 0)
	for k := range s.Storage.SwupChains {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// timestamp creates a merkle tree of all the latests skipblocks of each
// skipchains, run a timestamp protocol and store the results in
// s.latestTimestamps.
func (s *Service) timestamp(time time.Time) {
	measure := monitor.NewTimeMeasure("swup_timestamp")
	// order all packets and marshal them
	ids := s.orderedLatestSkipblocksID()
	// create merkle tree + proofs and the final message
	root, proofs := timestamp.ProofTree(HashFunc(), ids)
	msg := MarshalPair(root, time.Unix())
	// run protocol
	signature := s.cosiSign(msg)
	// TODO XXX Here in a non-academical world we should test if the
	// signature contains enough participants.
	s.updateTimestampInfo(root, proofs, time.Unix(), signature)
	measure.Record()
}

func (s *Service) cosiSign(msg []byte) []byte {
	sdaTree := s.Storage.Root.Roster.GenerateBinaryTree()

	tni := s.NewTreeNodeInstance(sdaTree, sdaTree.Root, swupdate.ProtocolName)
	pi, err := swupdate.NewCoSiUpdate(tni, s.cosiVerify)
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
	log.Lvl2("Waiting on cosi response ...")
	res := <-response
	log.Lvl2("... DONE: Recieved cosi response")
	return res
}

// cosiVerify takes the message from the cosi protocol and split it into
// the merkle tree root and the timestamp. It checks if the root is correct
// and if the timestamp is in a reasonable timeframe (s.ReasonableTime)
func (s *Service) cosiVerify(msg []byte) bool {
	signedRoot, signedTime := UnmarshalPair(msg)
	// check timestamp
	if time.Now().Sub(time.Unix(signedTime, 0)) > s.ReasonableTime {
		log.Lvl2("Timestamp is too far in the past")
		return false
	}
	// check merkle tree root
	// order all packets and marshal them
	ids := s.orderedLatestSkipblocksID()
	// create merkle tree + proofs and the final message
	root, _ := timestamp.ProofTree(HashFunc(), ids)
	// root of merkle tree is not secret, no need to use constant time.
	if !bytes.Equal(root, signedRoot) {
		log.Lvl2("Root of merkle root does not match")
		return false
	}

	log.Lvl3("Swupdate cosi signature verified")
	return true
}

// orderedLatestSkipblocksID sorts the latests blocks of all skipchains and
// return all ids in an array of HashID
func (s *Service) orderedLatestSkipblocksID() []timestamp.HashID {
	keys := s.getOrderedPackageNames()

	ids := make([]timestamp.HashID, 0)
	chains := s.Storage.SwupChains
	for _, key := range keys {
		ids = append(ids, timestamp.HashID(chains[key].Data.Hash))
	}
	return ids
}

// newSwupdate create a new service and tries to load an eventually
// already existing one.
func newSwupdate(c *onet.Context) onet.Service {
	s := &Service{
		ServiceProcessor: onet.NewServiceProcessor(c),
		skipchain:        skipchain.NewClient(),
		Storage: &storage{
			SwupChains:        map[string]*SwupChain{},
			SwupChainsGenesis: map[string]*SwupChain{},
		},
		// default value
		ReasonableTime: time.Hour,
	}
	err := s.RegisterHandlers(s.CreatePackage,
		s.UpdatePackage,
		s.TimestampProof,
		s.LatestBlocks,
		s.TimestampProofs)
	if err != nil {
		log.ErrFatal(err, "Couldn't register message")
	}
	return s
}

func (s *Service) updateTimestampInfo(rootID timestamp.HashID, proofs []timestamp.Proof, ts int64, sig []byte) {
	s.Lock()
	defer s.Unlock()
	if s.Storage.Timestamp == nil {
		s.Storage.Timestamp = &Timestamp{}
	}
	var t = s.Storage.Timestamp
	t.Timestamp = ts
	t.Root = rootID
	t.Signature = sig
	t.Proofs = proofs
}

// HashFunc used for the timestamp operations with the Merkle tree generation
// and verification.
func HashFunc() timestamp.HashFunc {
	return sha256.New
}

// MarshalPair takes the root of a merkle tree (only a slice of bytes) and a
// unix timestamp and marshal them. UnmarshalPair do the opposite.
func MarshalPair(root timestamp.HashID, time int64) []byte {
	var buff bytes.Buffer
	if err := binary.Write(&buff, binary.BigEndian, time); err != nil {
		panic(err)
	}
	return append(buff.Bytes(), []byte(root)...)
}

// UnmarshalPair takes a slice of bytes generated by MarshalPair and retrieve
// the root and the unix timestamp out of it.
func UnmarshalPair(buff []byte) (timestamp.HashID, int64) {
	var reader = bytes.NewBuffer(buff)
	var time int64
	if err := binary.Read(reader, binary.BigEndian, &time); err != nil {
		panic(err)
	}
	return reader.Bytes(), time
}
