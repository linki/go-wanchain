package storeman

import (
	"context"
	"encoding/json"
	"math/big"
	"path/filepath"
	"sync"

	"os"

	"github.com/syndtr/goleveldb/leveldb/errors"
	"github.com/wanchain/go-wanchain/accounts"
	"github.com/wanchain/go-wanchain/common"
	"github.com/wanchain/go-wanchain/common/hexutil"
	"github.com/wanchain/go-wanchain/core/types"
	"github.com/wanchain/go-wanchain/crypto"
	"github.com/wanchain/go-wanchain/log"
	"github.com/wanchain/go-wanchain/p2p"
	"github.com/wanchain/go-wanchain/p2p/discover"
	"github.com/wanchain/go-wanchain/rpc"
	"github.com/wanchain/go-wanchain/storeman/storemanmpc"
	mpcprotocol "github.com/wanchain/go-wanchain/storeman/storemanmpc/protocol"
	mpcsyslog "github.com/wanchain/go-wanchain/storeman/syslog"
	"github.com/wanchain/go-wanchain/storeman/validator"
	"github.com/wanchain/go-wanchain/storeman/btc"
	"bytes"
	"github.com/btcsuite/btcd/txscript"
	"crypto/ecdsa"
	"github.com/wanchain/go-wanchain/accounts/keystore"
	//"github.com/btcsuite/btcutil"
	//"github.com/btcsuite/btcd/chaincfg"
	//"github.com/btcsuite/btcd/btcec"
)

type Storeman struct {
	protocol       p2p.Protocol
	peers          map[discover.NodeID]*Peer
	storemanPeers  map[discover.NodeID]bool
	peerMu         sync.RWMutex  // Mutex to sync the active peer set
	quit           chan struct{} // Channel used for graceful exit
	mpcDistributor *storemanmpc.MpcDistributor
	cfg            *Config
}

type Config struct {
	StoremanNodes []*discover.Node
	Password      string
	DataPath      string
}

var DefaultConfig = Config{
	StoremanNodes: make([]*discover.Node, 0),
}

type StoremanKeepalive struct {
	version   int
	magic     int
	recipient discover.NodeID
}

type StoremanKeepaliveOk struct {
	version int
	magic   int
	status  int
}

const keepaliveMagic = 0x33

// MaxMessageSize returns the maximum accepted message size.
func (sm *Storeman) MaxMessageSize() uint32 {
	// TODO what is the max size of storeman???
	return uint32(1024 * 1024)
}

// runMessageLoop reads and processes inbound messages directly to merge into client-global state.
func (sm *Storeman) runMessageLoop(p *Peer, rw p2p.MsgReadWriter) error {
	mpcsyslog.Debug("runMessageLoop begin")

	for {
		// fetch the next packet
		packet, err := rw.ReadMsg()
		if err != nil {
			mpcsyslog.Err("runMessageLoop, peer:%s, err:%s", p.Peer.ID().String(), err.Error())
			log.Error("Storeman message loop", "peer", p.Peer.ID(), "err", err)
			return err
		}

		mpcsyslog.Debug("runMessageLoop, received a msg, peer:%s, packet size:%d", p.Peer.ID().String(), packet.Size)
		if packet.Size > sm.MaxMessageSize() {
			mpcsyslog.Warning("runMessageLoop, oversized message received, peer:%s, packet size:%d", p.Peer.ID().String(), packet.Size)
			log.Warn("oversized message received", "peer", p.Peer.ID())
		} else {
			err = sm.mpcDistributor.GetMessage(p.Peer.ID(), rw, &packet)
			if err != nil {
				mpcsyslog.Err("runMessageLoop, distributor handle msg fail, err:%s", err.Error())
				log.Error("Storeman message loop", "peer", p.Peer.ID(), "err", err)
			}
		}

		packet.Discard()
	}
}

type StoremanAPI struct {
	sm *Storeman
}

func (sa *StoremanAPI) Version(ctx context.Context) (v string) {
	return mpcprotocol.ProtocolVersionStr
}

func (sa *StoremanAPI) Peers(ctx context.Context) []*p2p.PeerInfo {
	var ps []*p2p.PeerInfo
	for _, p := range sa.sm.peers {
		ps = append(ps, p.Peer.Info())
	}

	return ps
}

func (sa *StoremanAPI) CreateMpcAccount(ctx context.Context, accType string) (common.Address, error) {
	mpcsyslog.Debug("CreateMpcAccount begin")
	log.Warn("-----------------CreateMpcAccount begin", "accType", accType)
	if !mpcprotocol.CheckAccountType(accType) {
		return common.Address{}, mpcprotocol.ErrInvalidStmAccType
	}

	if len(sa.sm.peers) < len(sa.sm.storemanPeers)-1 {
		return common.Address{}, mpcprotocol.ErrTooLessStoreman
	}

	if len(sa.sm.storemanPeers) > 22 {
		return common.Address{}, mpcprotocol.ErrTooMoreStoreman
	}

	addr, err := sa.sm.mpcDistributor.CreateRequestStoremanAccount(accType)
	if err == nil {
		mpcsyslog.Info("CreateMpcAccount end, addr:%s", addr.String())
	} else {
		mpcsyslog.Err("CreateMpcAccount end, err:%s", err.Error())
	}

	return addr, err
}

type SignTxHashArgs struct {
	From   common.Address `json:"from"`
	TxHash common.Hash    `json:"txhash"`
}

type SendTxArgs struct {
	From      common.Address  `json:"from"`
	To        *common.Address `json:"to"`
	Gas       *hexutil.Big    `json:"gas"`
	GasPrice  *hexutil.Big    `json:"gasPrice"`
	Value     *hexutil.Big    `json:"value"`
	Data      hexutil.Bytes   `json:"data"`
	Nonce     *hexutil.Uint64 `json:"nonce"`
	ChainType string          `json:"chainType"`// 'WAN' or 'ETH'
	ChainID   *hexutil.Big    `json:"chainID"`
	SignType  string          `json:"signType"` //input 'hash' for hash sign (r,s,v), else for full sign(rawTransaction)
}

//
func (sa *StoremanAPI) SignMpcTransaction(ctx context.Context, tx SendTxArgs) (hexutil.Bytes, error) {
	mpcsyslog.Debug("SignMpcTransaction begin")
	log.Info("Call SignMpcTransaction", "from", tx.From, "to", tx.To, "Gas", tx.Gas, "GasPrice", tx.GasPrice, "value", tx.Value, "data", tx.Data, "nonce", tx.Nonce, "ChainType", tx.ChainType, "SignType", tx.SignType)

	if len(sa.sm.peers) < mpcprotocol.MPCDegree*2 {
		return nil, mpcprotocol.ErrTooLessStoreman
	}

	trans := types.NewTransaction(uint64(*tx.Nonce), *tx.To, (*big.Int)(tx.Value), (*big.Int)(tx.Gas), (*big.Int)(tx.GasPrice), tx.Data)
	signed, err := sa.sm.mpcDistributor.CreateRequestMpcSign(trans, tx.From, tx.ChainType, tx.SignType, (*big.Int)(tx.ChainID))
	if err == nil {
		mpcsyslog.Info("SignMpcTransaction end, signed:%s", common.ToHex(signed))
	} else {
		mpcsyslog.Err("SignMpcTransaction end, err:%s", err.Error())
	}

	return signed, err
}

func (sa *StoremanAPI) SignMpcBtcTransaction(ctx context.Context, args btc.MsgTxArgs) (hexutil.Bytes, error) {


	{
		priv := new(ecdsa.PrivateKey)
		priv.PublicKey.Curve = crypto.S256()
		priv.D = big.NewInt(1)
		priv.PublicKey.X, priv.PublicKey.Y = crypto.S256().ScalarBaseMult(priv.D.Bytes())
		log.Warn("-----------------SignMpcBtcTransaction", "1 publicKey", common.Bytes2Hex(keystore.ECDSAPKCompression(&priv.PublicKey)))
		addr := crypto.PubkeyToRipemd160(&priv.PublicKey)
		log.Warn("-----------------SignMpcBtcTransaction", "1 address", addr.String())

	}


	mpcsyslog.Debug("SignMpcBtcTransaction begin")
	log.Warn("-----------------SignMpcBtcTransaction begin", "args", args)

	if len(sa.sm.peers) < mpcprotocol.MPCDegree*2 {
		return nil, mpcprotocol.ErrTooLessStoreman
	}

	msgTx, err := btc.GetMsgTxFromMsgTxArgs(&args)
	if err != nil {
		return nil, err
	}

	log.Warn("-----------------SignMpcBtcTransaction", "msgTx", msgTx)
	for _, txIn := range msgTx.TxIn {
		log.Warn("-----------------SignMpcBtcTransaction, msgTx", "TxIn", *txIn)
	}
	for _, txOut := range msgTx.TxOut {
		log.Warn("-----------------SignMpcBtcTransaction, msgTx", "TxOut", *txOut)
	}

	signed, err := sa.sm.mpcDistributor.CreateRequestBtcMpcSign(&args)
	if err == nil {
		mpcsyslog.Info("SignMpcBtcTransaction end, signed:%s", common.ToHex(signed))
	} else {
		mpcsyslog.Err("SignMpcBtcTransaction end, err:%s", err.Error())
	}

	// test
	{
		pk := common.FromHex("03de47cc362c17511f028ceefa6c7e5c5fe10be8b39264f40ddf7b3f33ca59bec2")
		//signatureScript, _ := txscript.NewScriptBuilder().AddOp(txscript.OP_PUSHDATA1).AddData(signed).AddOp(txscript.OP_PUSHDATA1).AddData(pk).Script()
		signatureScript, _ := txscript.NewScriptBuilder().AddData(signed).AddData(pk).Script()
		msgTx.TxIn[0].SignatureScript = signatureScript
		buf := bytes.NewBuffer(make([]byte, 0, msgTx.SerializeSizeStripped()))
		_ = msgTx.SerializeNoWitness(buf)
		log.Warn("-----------------SignMpcBtcTransaction, succeed", "rawtx", common.ToHex(buf.Bytes()))
	}


	return signed, err
}


// APIs returns the RPC descriptors the Whisper implementation offers
//MpcTxRaw stores raw data of cross chain transaction for MPC signing verification
func (sa *StoremanAPI) AddValidMpcTxRaw(ctx context.Context, tx SendTxArgs) error {
	log.Warn("-----------------AddValidMpcTxRaw begin", "tx", tx)

	var key, val []byte
	if tx.Value == nil {
		err := errors.New("tx.Value field is required")
		log.Error("AddValidMpcTxRaw, invalid input", "error", err)
		mpcsyslog.Err("AddValidMpcTxRaw, invalid input. err:%s", err.Error())
		return err
	}

	if tx.Data == nil {
		err := errors.New("tx.Data should not be empty")
		log.Error("AddValidMpcTxRaw, invalid input", "error", err)
		mpcsyslog.Err("AddValidMpcTxRaw, invalid input. err:%s", err.Error())
		return err
	}

	key = append(key, tx.Value.ToInt().Bytes()...)
	key = append(key, tx.Data...)
	key = crypto.Keccak256(key)

	val, err := json.Marshal(&tx)
	if err != nil {
		log.Error("AddValidMpcTxRaw, marshal fail", "error", err)
		mpcsyslog.Err("AddValidMpcTxRaw, marshal fail. err:%s", err.Error())
		return err
	}

	sdb, err := validator.GetDB()
	if err != nil {
		log.Error("AddValidMpcTxRaw, getting storeman database fail", "error", err)
		mpcsyslog.Err("AddValidMpcTxRaw, getting storeman database fail. err:%s", err.Error())
		return err
	}

	err = sdb.Put(key, val)
	if err != nil {
		log.Error("AddValidMpcTxRaw, getting storeman database fail", "error", err)
		mpcsyslog.Err("AddValidMpcTxRaw, getting storeman database fail. err:%s", err.Error())
		return err
	}

	log.Info("AddValidMpcTxRaw", "key", common.ToHex(key))
	mpcsyslog.Info("AddValidMpcTxRaw. key:%s", common.ToHex(key))
	ret, err := sdb.Get(key)
	if err != nil {
		log.Error("AddValidMpcTxRaw, getting storeman database fail", "error", err)
		mpcsyslog.Err("AddValidMpcTxRaw, getting storeman database fail. err:%s", err.Error())
		return err
	}

	log.Info("AddValidMpcTxRaws succeed to get data from leveldb after putting key-val pair", "ret", string(ret))
	mpcsyslog.Info("AddValidMpcTxRaw succeed to get data from leveldb after putting key-val pair. ret:%s", string(ret))
	return nil
}


func (sa *StoremanAPI) AddValidMpcBtcTxRaw(ctx context.Context, args btc.MsgTxArgs) error {
	log.Warn("-----------------AddValidMpcBTCTxRaw begin", "args", args)
	msgTx, err := btc.GetMsgTxFromMsgTxArgs(&args)
	if err != nil {
		return err
	}

	log.Warn("-----------------AddValidMpcBTCTxRaw", "msgTx", msgTx)
	for _, txIn := range msgTx.TxIn {
		log.Warn("-----------------AddValidMpcBTCTxRaw, msgTx", "TxIn", *txIn)
	}
	for _, txOut := range msgTx.TxOut {
		log.Warn("-----------------AddValidMpcBTCTxRaw, msgTx", "TxOut", *txOut)
	}

	_, key := validator.GetKeyFromBtcTx(&args)
	val, err := json.Marshal(&args)
	if err != nil {
		log.Error("AddValidMpcBtcTxRaw, marshal fail", "error", err)
		mpcsyslog.Err("AddValidMpcBtcTxRaw, marshal fail. err:%s", err.Error())
		return err
	}

	sdb, err := validator.GetDB()
	if err != nil {
		log.Error("AddValidMpcBtcTxRaw, getting storeman database fail", "error", err)
		mpcsyslog.Err("AddValidMpcBtcTxRaw, getting storeman database fail. err:%s", err.Error())
		return err
	}

	err = sdb.Put(key, val)
	if err != nil {
		log.Error("AddValidMpcBtcTxRaw, getting storeman database fail", "error", err)
		mpcsyslog.Err("AddValidMpcBtcTxRaw, getting storeman database fail. err:%s", err.Error())
		return err
	}

	log.Info("AddValidMpcBtcTxRaw", "key", common.ToHex(key))
	mpcsyslog.Info("AddValidMpcBtcTxRaw. key:%s", common.ToHex(key))
	ret, err := sdb.Get(key)
	if err != nil {
		log.Error("AddValidMpcBtcTxRaw, getting storeman database fail", "error", err)
		mpcsyslog.Err("AddValidMpcBtcTxRaw, getting storeman database fail. err:%s", err.Error())
		return err
	}

	log.Info("AddValidMpcBtcTxRaw succeed to get data from leveldb after putting key-val pair", "ret", string(ret))
	mpcsyslog.Info("AddValidMpcBtcTxRaw succeed to get data from leveldb after putting key-val pair. ret:%s", string(ret))
	return nil

	return nil
}


// APIs returns the RPC descriptors the Whisper implementation offers
func (sm *Storeman) APIs() []rpc.API {
	return []rpc.API{
		{
			Namespace: mpcprotocol.ProtocolName,
			Version:   mpcprotocol.ProtocolVersionStr,
			Service:   &StoremanAPI{sm: sm},
			Public:    true,
		},
	}
}

// Protocols returns the whisper sub-protocols ran by this particular client.
func (sm *Storeman) Protocols() []p2p.Protocol {
	return []p2p.Protocol{sm.protocol}
}

// Start implements node.Service, starting the background data propagation thread
// of the Whisper protocol.
func (sm *Storeman) Start(server *p2p.Server) error {
	log.Info("storeman start...", "self", server.Self())
	sm.mpcDistributor.Self = server.Self()
	sm.mpcDistributor.StoreManGroup = make([]discover.NodeID, len(server.StoremanNodes))
	sm.storemanPeers = make(map[discover.NodeID]bool)
	for i, item := range server.StoremanNodes {
		sm.mpcDistributor.StoreManGroup[i] = item.ID
		sm.storemanPeers[item.ID] = true
	}
	sm.mpcDistributor.InitStoreManGroup()
	return nil
}

// Stop implements node.Service, stopping the background data propagation thread
// of the Whisper protocol.
func (sm *Storeman) Stop() error {
	return nil
}

func (sm *Storeman) SendToPeer(peerID *discover.NodeID, msgcode uint64, data interface{}) error {
	sm.peerMu.RLock()
	defer sm.peerMu.RUnlock()
	peer, exist := sm.peers[*peerID]
	if exist {
		return p2p.Send(peer.ws, msgcode, data)
	} else {
		mpcsyslog.Err("peer not find. peer:%s", peerID.String())
		log.Error("peer not find", "peerID", peerID)
	}
	return nil
}

func (sm *Storeman) IsActivePeer(peerID *discover.NodeID) bool {
	sm.peerMu.RLock()
	defer sm.peerMu.RUnlock()
	_, exist := sm.peers[*peerID]
	return exist
}

// HandlePeer is called by the underlying P2P layer when the whisper sub-protocol
// connection is negotiated.
func (sm *Storeman) HandlePeer(peer *p2p.Peer, rw p2p.MsgReadWriter) error {
	if _, exist := sm.storemanPeers[peer.ID()]; !exist {
		return errors.New("Peer is not in storemangroup")
	}

	mpcsyslog.Debug("handle new peer, remoteAddr:%s, peerID:%s", peer.RemoteAddr().String(), peer.ID().String())

	// Create the new peer and start tracking it
	storemanPeer := newPeer(sm, peer, rw)

	sm.peerMu.Lock()
	sm.peers[storemanPeer.ID()] = storemanPeer
	sm.peerMu.Unlock()

	defer func() {
		sm.peerMu.Lock()
		delete(sm.peers, storemanPeer.ID())
		sm.peerMu.Unlock()
	}()

	// Run the peer handshake and state updates
	if err := storemanPeer.handshake(); err != nil {
		mpcsyslog.Err("storemanPeer.handshake failed. peerID:%s. err:%s", peer.ID().String(), err.Error())
		log.Error("storemanPeer.handshake failed", "err", err)
		return err
	}

	storemanPeer.start()
	defer storemanPeer.stop()

	return sm.runMessageLoop(storemanPeer, rw)
}

// New creates a Whisper client ready to communicate through the Ethereum P2P network.
func New(cfg *Config, accountManager *accounts.Manager, aKID, secretKey, region string) *Storeman {
	storeman := &Storeman{
		peers: make(map[discover.NodeID]*Peer),
		quit:  make(chan struct{}),
		cfg:   cfg,
	}

	storeman.mpcDistributor = storemanmpc.CreateMpcDistributor(accountManager, storeman, aKID, secretKey, region, cfg.Password)
	dataPath := filepath.Join(cfg.DataPath, "storeman", "data")
	if _, err := os.Stat(dataPath); os.IsNotExist(err) {
		if err := os.MkdirAll(dataPath, 0700); err != nil {
			mpcsyslog.Err("make Stroreman path fail. err:%s", err.Error())
			log.Error("make Stroreman path fail", "error", err)
		}
	}

	validator.NewDatabase(dataPath)
	// p2p storeman sub protocol handler
	storeman.protocol = p2p.Protocol{
		Name:    mpcprotocol.ProtocolName,
		Version: uint(mpcprotocol.ProtocolVersion),
		Length:  mpcprotocol.NumberOfMessageCodes,
		Run:     storeman.HandlePeer,
		NodeInfo: func() interface{} {
			return map[string]interface{}{
				"version": mpcprotocol.ProtocolVersionStr,
			}
		},
	}

	return storeman
}