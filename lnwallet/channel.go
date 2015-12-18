package wallet

import (
	"sync"

	"li.lan/labs/plasma/chainntfs"
	"li.lan/labs/plasma/revocation"

	"github.com/btcsuite/btcd/btcec"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/btcsuite/btcutil"
	"github.com/btcsuite/btcwallet/walletdb"
)

const (
	// TODO(roasbeef): make not random value
	MaxPendingPayments = 10
)

// P2SHify...
func P2SHify(scriptBytes []byte) ([]byte, error) {
	bldr := txscript.NewScriptBuilder()
	bldr.AddOp(txscript.OP_HASH160)
	bldr.AddData(btcutil.Hash160(scriptBytes))
	bldr.AddOp(txscript.OP_EQUAL)
	return bldr.Script()
}

//TODO(j): Creates a CLTV-only funding Tx (reserve is *REQUIRED*)
//This works for only CLTV soft-fork (no CSV/segwit soft-fork in yet)
//
//Commit funds to Funding Tx, will timeout after the fundingTimeLock and refund
//back using CLTV. As there is no way to enforce HTLCs, we rely upon a reserve
//and have each party's HTLCs in-transit be less than their Commitment reserve.
//In the event that someone incorrectly broadcasts an old Commitment TX, then
//the counterparty claims the full reserve. It may be possible for either party
//to claim the HTLC(!!! But it's okay because the "honest" party is made whole
//via the reserve). If it's two-funder there are two outputs and the
//Commitments spends from both outputs in the Funding Tx. Two-funder requires
//the ourKey/theirKey sig positions to be swapped (should be in 1 funding tx).
//
//Quick note before I forget: The revocation hash is used in CLTV-only for
//single-funder (without an initial payment) *as part of an additional output
//in the Commitment Tx for the reserve*. This is to establish a unidirectional
//channel UNITL the recipient has sufficient funds. When the recipient has
//sufficient funds, the revocation is exchanged and allows the recipient to
//claim the full reserve as penalty if the incorrect Commitment is broadcast
//(otherwise it's timelocked refunded back to the sender). From then on, there
//is no additional output in Commitment Txes. [side caveat, first payment must
//be above minimum UTXO output size in single-funder] For now, let's keep it
//simple and assume dual funder (with both funding above reserve)
func createCLTVFundingTx(fundingTimeLock int64, ourKey *btcec.PublicKey, theirKey *btcec.PublicKey) (*wire.MsgTx, error) {
	script := txscript.NewScriptBuilder()
	//See how many entries there are
	//2: it's a 2-of-2 multisig
	//anything else: assume it's a CLTV-timeout 1-sig only
	script.AddOp(txscript.OP_DEPTH)
	script.AddInt64(2)
	script.AddOp(txscript.OP_EQUAL)

	//If this is a 2-of-2 multisig, read the first sig
	script.AddOp(txscript.OP_IF)
	//Sig2 (not P2PKH, the pubkey is in the redeemScript)
	script.AddData(ourKey.SerializeCompressed())
	script.AddOp(txscript.OP_CHECKSIGVERIFY) //gotta be verify!

	//If this is timed out
	script.AddOp(txscript.OP_ELSE)
	script.AddInt64(fundingTimeLock)
	script.AddOp(txscript.OP_NOP2) //CLTV
	//Sig (not P2PKH, the pubkey is in the redeemScript)
	script.AddOp(txscript.OP_CHECKSIG)
	script.AddOp(txscript.OP_DROP)
	script.AddOp(txscript.OP_ENDIF)

	//Read the other sig if it's 2-of-2, only one if it's timed out
	script.AddData(theirKey.SerializeCompressed())
	script.AddOp(txscript.OP_CHECKSIG)

	fundingTx := wire.NewMsgTx()
	//TODO(j) Add the inputs/outputs

	return fundingTx, nil
}

// createCommitTx...
func createCommitTx(fundingOutput *wire.TxIn, ourKey, theirKey *btcec.PublicKey,
	revokeHash [32]byte, csvTimeout int64, amtToUs,
	amtToThem btcutil.Amount) (*wire.MsgTx, error) {

	// First, we create the script paying to us. This script is spendable
	// under two conditions: either the 'csvTimeout' has passed and we can
	// redeem our funds, or they have the pre-image to 'revokeHash'.
	scriptToUs := txscript.NewScriptBuilder()
	scriptToUs.AddOp(txscript.OP_HASH160)
	scriptToUs.AddData(revokeHash[:])
	scriptToUs.AddOp(txscript.OP_EQUAL)
	scriptToUs.AddOp(txscript.OP_IF)
	scriptToUs.AddData(theirKey.SerializeCompressed())
	scriptToUs.AddOp(txscript.OP_ELSE)
	scriptToUs.AddInt64(csvTimeout)
	scriptToUs.AddOp(txscript.OP_NOP3) // CSV
	scriptToUs.AddOp(txscript.OP_DROP)
	scriptToUs.AddData(ourKey.SerializeCompressed())
	scriptToUs.AddOp(txscript.OP_ENDIF)
	scriptToUs.AddOp(txscript.OP_CHECKSIG)

	// TODO(roasbeef): store
	ourRedeemScript, err := scriptToUs.Script()
	if err != nil {
		return nil, err
	}
	payToUsScriptHash, err := P2SHify(ourRedeemScript)
	if err != nil {
		return nil, err
	}

	// Next, we create the script paying to them. This is just a regular
	// P2PKH (wrapped in P2SH) with their key.
	scriptToThem := txscript.NewScriptBuilder()
	scriptToThem.AddOp(txscript.OP_DUP)
	scriptToThem.AddOp(txscript.OP_HASH160)
	scriptToThem.AddData(btcutil.Hash160(theirKey.SerializeCompressed()))
	scriptToThem.AddOp(txscript.OP_EQUALVERIFY)
	scriptToThem.AddOp(txscript.OP_CHECKSIG)

	theirRedeemScript, err := scriptToThem.Script()
	if err != nil {
		return nil, err
	}
	payToThemScriptHash, err := P2SHify(theirRedeemScript)
	if err != nil {
		return nil, err
	}

	// TODO(roasbeef): sort outputs?
	commitTx := wire.NewMsgTx()
	commitTx.AddTxIn(fundingOutput)
	commitTx.AddTxOut(wire.NewTxOut(int64(amtToUs), payToUsScriptHash))
	commitTx.AddTxOut(wire.NewTxOut(int64(amtToThem), payToThemScriptHash))

	return commitTx, nil
}

type nodeId [32]byte

type LightningChannel struct {
	wallet        *LightningWallet
	channelEvents *chainntnfs.ChainNotifier

	// TODO(roasbeef): Stores all previous R values + timeouts for each
	// commitment update, plus some other meta-data...Or just use OP_RETURN
	// to help out?
	channelNamespace walletdb.Namespace

	Id [32]byte

	capacity        btcutil.Amount
	ourTotalFunds   btcutil.Amount
	theirTotalFunds btcutil.Amount

	// TODO(roasbeef): another shachain for R values for HTLC's?
	// solve above?
	ourShaChain   *revocation.HyperShaChain
	theirShaChain *revocation.HyperShaChain

	ourFundingKey   *btcec.PrivateKey
	theirFundingKey *btcec.PublicKey

	ourCommitKey   *btcec.PublicKey
	theirCommitKey *btcec.PublicKey

	fundingTx    *wire.MsgTx
	commitmentTx *wire.MsgTx

	sync.RWMutex

	// TODO(roasbeef): create and embed 'Service' interface w/ below?
	started  int32
	shutdown int32

	quit chan struct{}
	wg   sync.WaitGroup
}

// newLightningChannel...
// TODO(roasbeef): bring back and embedd CompleteReservation struct? kinda large
// atm.....
func newLightningChannel(wallet *LightningWallet, events *chainntnfs.ChainNotifier,
	dbNamespace walletdb.Namespace, theirNodeID nodeId, ourFunds,
	theirFunds btcutil.Amount, ourChain, theirChain *revocation.HyperShaChain,
	ourFundingKey *btcec.PrivateKey, theirFundingKey *btcec.PublicKey,
	commitKey, theirCommitKey *btcec.PublicKey, fundingTx,
	commitTx *wire.MsgTx) (*LightningChannel, error) {

	return &LightningChannel{
		wallet:           wallet,
		channelEvents:    events,
		channelNamespace: dbNamespace,
		Id:               theirNodeID,
		capacity:         ourFunds + theirFunds,
		ourTotalFunds:    ourFunds,
		theirTotalFunds:  theirFunds,
		ourShaChain:      ourChain,
		theirShaChain:    theirChain,
		ourFundingKey:    ourFundingKey,
		theirFundingKey:  theirFundingKey,
		ourCommitKey:     commitKey,
		theirCommitKey:   theirCommitKey,
		fundingTx:        fundingTx,
		commitmentTx:     commitTx,
	}, nil
}

// AddHTLC...
func (lc *LightningChannel) AddHTLC() {
}

// SettleHTLC...
func (lc *LightningChannel) SettleHTLC() {
}

// OurBalance...
func (lc *LightningChannel) OurBalance() btcutil.Amount {
	return 0
}

// TheirBalance...
func (lc *LightningChannel) TheirBalance() btcutil.Amount {
	return 0
}

// CurrentCommitTx...
func (lc *LightningChannel) CurrentCommitTx() *btcutil.Tx {
	return nil
}

// SignTheirCommitTx...
func (lc *LightningChannel) SignTheirCommitTx(commitTx *btcutil.Tx) error {
	return nil
}

// AddTheirSig...
func (lc *LightningChannel) AddTheirSig(sig []byte) error {
	return nil
}

// VerifyCommitmentUpdate...
func (lc *LightningChannel) VerifyCommitmentUpdate() error {
	return nil
}