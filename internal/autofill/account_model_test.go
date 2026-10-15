package autofill

import (
	"bytes"
	"encoding/binary"
	"math"
	mrand "math/rand"
	"testing"

	"github.com/ethereum/go-ethereum/crypto"
)

func TestParseAccountModel(t *testing.T) {
	for _, in := range []string{"", "normal"} {
		if got, err := ParseAccountModel(in); err != nil || got != nil {
			t.Errorf("ParseAccountModel(%q) = %v, %v; want nil, nil", in, got, err)
		}
	}
	if got, err := ParseAccountModel("mainnet"); err != nil || got == nil {
		t.Errorf("ParseAccountModel(mainnet) = %v, %v; want a model", got, err)
	}
	if _, err := ParseAccountModel("heavy"); err == nil {
		t.Error("ParseAccountModel(heavy) should error")
	}
}

// binomialTolerance returns a 6-sigma absolute tolerance for a binomial(n, p) count.
func binomialTolerance(n int, p float64) float64 {
	return 6 * math.Sqrt(float64(n)*p*(1-p))
}

// Draws must reproduce the table's class shares and the eoa class's zero-nonce/zero-balance shares.
func TestMainnetEOADrawsMatchCalibration(t *testing.T) {
	am := newMainnetAccountModel()
	rng := mrand.New(mrand.NewSource(1))
	const n = 300_000

	var eoaN, delegationN, contractN int
	var eoaNonceZero, eoaBalanceZero int
	for range n {
		acc := am.drawEOA(rng, false)
		if len(acc.Storage) != 0 {
			t.Fatal("EOA draw must not carry storage")
		}
		switch {
		case len(acc.Code) == 0:
			eoaN++
			if acc.StateAccount.Nonce == 0 {
				eoaNonceZero++
			}
			if acc.StateAccount.Balance.IsZero() {
				eoaBalanceZero++
			}
		case len(acc.Code) == 23 && bytes.Equal(acc.Code[:3], delegationPrefix):
			delegationN++
			if acc.CodeHash != crypto.Keccak256Hash(acc.Code) {
				t.Fatal("delegation CodeHash does not match its code")
			}
		default:
			contractN++
			if acc.Code[0] == 0xEF {
				t.Fatalf("contract-class code must never start with 0xEF: %x", acc.Code[:4])
			}
			if acc.CodeHash != crypto.Keccak256Hash(acc.Code) {
				t.Fatal("contract CodeHash does not match its code")
			}
		}
	}

	total := float64(am.eoaClassCum[2])
	check := func(name string, got int, wantCount uint64) {
		t.Helper()
		p := float64(wantCount) / total
		if tol := binomialTolerance(n, p); math.Abs(float64(got)-p*n) > tol {
			t.Errorf("%s: got %d draws, want %.0f ± %.0f", name, got, p*n, tol)
		}
	}
	check("eoa", eoaN, am.eoaClassCum[0])
	check("delegation", delegationN, am.eoaClassCum[1]-am.eoaClassCum[0])
	check("contract", contractN, am.eoaClassCum[2]-am.eoaClassCum[1])

	for _, c := range []struct {
		name string
		got  int
		d    lenDist
	}{{"eoa nonce==0", eoaNonceZero, am.nonceLen[0]}, {"eoa balance==0", eoaBalanceZero, am.balanceLen[0]}} {
		p := float64(c.d.cum[0]) / float64(c.d.total)
		if tol := binomialTolerance(eoaN, p); math.Abs(float64(c.got)-p*float64(eoaN)) > tol {
			t.Errorf("%s: got %d, want %.0f ± %.0f", c.name, c.got, p*float64(eoaN), tol)
		}
	}
}

// The storage-less contract code draw must reproduce the top-1/top-10/top-1024 shares (jochemnet:
// ~35%, ~81%, ~98.3% of those accounts).
func TestMainnetContractCodeSharesMatchCalibration(t *testing.T) {
	cm := newMainnetCodeModel()
	rng := mrand.New(mrand.NewSource(2))
	const n = 300_000
	var top1, top10, top1024 int
	for range n {
		rank := cm.draw(rng)
		if rank < 1 {
			top1++
		}
		if rank < 10 {
			top10++
		}
		if rank < len(mainnetTopCodes) {
			top1024++
		}
	}
	check := func(name string, got int, weight uint64) {
		t.Helper()
		p := float64(weight) / float64(cm.totalWeight)
		if tol := binomialTolerance(n, p); math.Abs(float64(got)-p*n) > tol {
			t.Errorf("%s: got %d draws, want %.0f ± %.0f", name, got, p*n, tol)
		}
	}
	check("top-1", top1, cm.topCum[0])
	check("top-10", top10, cm.topCum[9])
	check("top-1024", top1024, cm.topWeight)
}

// DrawStorageNonceBalance must reproduce contract_storage's two common nonce lengths.
func TestStorageDrawsMatchCalibration(t *testing.T) {
	am := newMainnetAccountModel()
	rng := mrand.New(mrand.NewSource(3))
	const n = 200_000
	hist := map[int]int{}
	for range n {
		nonce, _ := am.DrawStorageNonceBalance(rng)
		l := 0
		for x := nonce; x != 0; x >>= 8 {
			l++
		}
		hist[l]++
	}
	d := am.storageNonceLen
	for l := 0; l <= 1; l++ {
		count := d.cum[l]
		if l > 0 {
			count -= d.cum[l-1]
		}
		p := float64(count) / float64(d.total)
		if tol := binomialTolerance(n, p); math.Abs(float64(hist[l])-p*n) > tol {
			t.Errorf("nonce_len=%d: got %d, want %.0f ± %.0f", l, hist[l], p*n, tol)
		}
	}
}

// Two models fed the same seed must draw byte-identical accounts: the determinism every client's
// cross-root invariance depends on (each model has its own code cache).
func TestAccountModelTwoRunDeterminism(t *testing.T) {
	amA, amB := newMainnetAccountModel(), newMainnetAccountModel()
	rngA, rngB := mrand.New(mrand.NewSource(7)), mrand.New(mrand.NewSource(7))
	for i := range 5000 {
		a, b := amA.drawEOA(rngA, false), amB.drawEOA(rngB, false)
		if a.Address != b.Address || a.StateAccount.Nonce != b.StateAccount.Nonce ||
			a.StateAccount.Balance.Cmp(b.StateAccount.Balance) != 0 || !bytes.Equal(a.Code, b.Code) {
			t.Fatalf("draw %d diverged", i)
		}
	}
}

// AccountModel == nil must stay byte-identical to GenerateEOAFlavored.
func TestPlanDrawEOANormalModelUnaffected(t *testing.T) {
	p := &Plan{EOAFlavors: DefaultEOAFlavors()}
	rngPlan, rngDirect := mrand.New(mrand.NewSource(11)), mrand.New(mrand.NewSource(11))
	for i := range 2000 {
		got, want := p.DrawEOA(rngPlan), GenerateEOAFlavored(rngDirect, p.EOAFlavors)
		if got.Address != want.Address || got.StateAccount.Nonce != want.StateAccount.Nonce ||
			got.StateAccount.Balance.Cmp(want.StateAccount.Balance) != 0 || !bytes.Equal(got.Code, want.Code) {
			t.Fatalf("draw %d: Plan.DrawEOA(nil AccountModel) diverged from GenerateEOAFlavored", i)
		}
	}
}

// The tables must agree with each other, and the rest ranks must land on mainnetRestCodeLen
// exactly: every knot interval gets exactly its code count (the rank -> length map is onto).
func TestRestCodeLenReproducesKnots(t *testing.T) {
	cm := newMainnetCodeModel()
	if cm.totalWeight != mainnetAccountClasses[2].count {
		t.Fatalf("top + rest accounts = %d, contract class = %d", cm.totalWeight, mainnetAccountClasses[2].count)
	}
	k := mainnetRestCodeLen[:]
	if last := k[len(k)-1][1]; last != cm.restTotalCodes {
		t.Fatalf("rest code lengths cover %d codes, groups %d", last, cm.restTotalCodes)
	}
	got := make([]uint64, len(k))
	for rank := len(mainnetTopCodes); rank < len(mainnetTopCodes)+int(cm.restTotalCodes); rank++ {
		l := uint64(cm.codeLen(rank))
		i := 0
		for k[i][0] < l {
			i++
		}
		if i > 0 && l <= k[i-1][0] {
			t.Fatalf("rank %d: length %d below its knot interval", rank, l)
		}
		got[i]++
	}
	var below uint64
	for i, knot := range k {
		if want := knot[1] - below; got[i] != want {
			t.Errorf("knot %d (<= %d bytes): %d codes, want %d", i, knot[0], got[i], want)
		}
		below = knot[1]
	}
}

// codeForRank must never produce a leading 0xEF byte (EIP-7702 designator or EOF container).
func TestCodeForRankNeverStartsWithEF(t *testing.T) {
	const n = 5000
	guardHit := 0
	var preimage [len(codeDomain) + 8 + 4]byte
	copy(preimage[:], codeDomain)
	for rank := range n {
		binary.BigEndian.PutUint64(preimage[len(codeDomain):], uint64(rank))
		binary.BigEndian.PutUint32(preimage[len(codeDomain)+8:], 0)
		if crypto.Keccak256(preimage[:])[0] == 0xEF {
			guardHit++
		}
		if code := codeForRank(rank, 32); code[0] == 0xEF {
			t.Fatalf("rank %d: code starts with 0xEF", rank)
		}
	}
	if guardHit == 0 {
		t.Fatal("the 0xEF guard was never exercised in this sample; raise n")
	}
}
