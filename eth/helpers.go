//go:build !bignum_pure && !bignum_hol256
// +build !bignum_pure,!bignum_hol256

package eth

import (
	"crypto/sha256"
	_ "embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"math/bits"

	"github.com/protolambda/go-kzg/bls"
)

const (
	FIAT_SHAMIR_PROTOCOL_DOMAIN = "FSBLOBVERIFY_V1_"
)

type Polynomial []bls.Fr
type Polynomials [][]bls.Fr

var (
	// KZG CRS for G2
	kzgSetupG2 []bls.G2Point

	// KZG CRS for commitment computation
	kzgSetupLagrange []bls.G1Point

	// KZG CRS for G1 (only used in tests (for proof creation))
	KzgSetupG1 []bls.G1Point

	//go:embed trusted_setup.json
	kzgSetupStr string
)

type JSONTrustedSetup struct {
	SetupG1       []bls.G1Point `json:"setup_G1"`
	SetupG2       []bls.G2Point `json:"setup_G2"`
	SetupLagrange []bls.G1Point `json:"setup_G1_lagrange"`
}

// Initialize KZG subsystem (load the trusted setup data)
func init() {
	var parsedSetup = JSONTrustedSetup{}

	err := json.Unmarshal([]byte(kzgSetupStr), &parsedSetup)
	if err != nil {
		panic(err)
	}
	kzgSetupG2 = parsedSetup.SetupG2
	kzgSetupLagrange = bitReversalPermutation(parsedSetup.SetupLagrange)
	KzgSetupG1 = parsedSetup.SetupG1

	initDomain()
}

// Bit-reversal permutation helper functions

// Check if `value` is a power of two integer.
func isPowerOfTwo(value uint64) bool {
	return value > 0 && (value&(value-1) == 0)
}

// Reverse `order` bits of integer n
func reverseBits(n, order uint64) uint64 {
	if !isPowerOfTwo(order) {
		panic("Order must be a power of two.")
	}

	return bits.Reverse64(n) >> (65 - bits.Len64(order))
}

// Return a copy of the input array permuted by bit-reversing the indexes.
func bitReversalPermutation(l []bls.G1Point) []bls.G1Point {
	out := make([]bls.G1Point, len(l))

	order := uint64(len(l))

	for i := range l {
		out[i] = l[reverseBits(uint64(i), order)]
	}

	return out
}

// VerifyKZGProof implements verify_kzg_proof from the EIP-4844 consensus spec,
// only with the byte inputs already parsed into points & field elements.
func VerifyKZGProofFromPoints(polynomialKZG *bls.G1Point, z *bls.Fr, y *bls.Fr, kzgProof *bls.G1Point) bool {
	var zG2 bls.G2Point
	bls.MulG2(&zG2, &bls.GenG2, z)
	var yG1 bls.G1Point
	bls.MulG1(&yG1, &bls.GenG1, y)

	var xMinusZ bls.G2Point
	bls.SubG2(&xMinusZ, &kzgSetupG2[1], &zG2)
	var pMinusY bls.G1Point
	bls.SubG1(&pMinusY, polynomialKZG, &yG1)

	return bls.PairingsVerify(&pMinusY, &bls.GenG2, kzgProof, &xMinusZ)
}

// VerifyAggregateKZGProof implements verify_aggregate_kzg_proof from the EIP-4844 consensus spec,
// only operating on blobs that have already been converted into polynomials.
func VerifyAggregateKZGProofFromPolynomials(blobs Polynomials, expectedKZGCommitments KZGCommitmentSequence, kzgAggregatedProof KZGProof) (bool, error) {
	aggregatedPoly, aggregatedPolyCommitment, evaluationChallenge, err :=
		ComputeAggregatedPolyAndCommitment(blobs, expectedKZGCommitments)
	if err != nil {
		return false, err
	}
	y := EvaluatePolynomialInEvaluationForm(aggregatedPoly, evaluationChallenge)
	kzgProofG1, err := bls.FromCompressedG1(kzgAggregatedProof[:])
	if err != nil {
		return false, fmt.Errorf("failed to decode kzgProof: %v", err)
	}
	return VerifyKZGProofFromPoints(aggregatedPolyCommitment, evaluationChallenge, y, kzgProofG1), nil
}

// ComputePowers implements compute_powers from the EIP-4844 consensus spec:
// https://github.com/ethereum/consensus-specs/blob/dev/specs/eip4844/polynomial-commitments.md#compute_powers
func ComputePowers(r *bls.Fr, n int) []bls.Fr {
	var currentPower bls.Fr
	bls.AsFr(&currentPower, 1)
	powers := make([]bls.Fr, n)
	for i := range powers {
		powers[i] = currentPower
		bls.MulModFr(&currentPower, &currentPower, r)
	}
	return powers
}

func PolynomialToKZGCommitment(eval Polynomial) KZGCommitment {
	g1 := bls.LinCombG1(kzgSetupLagrange, []bls.Fr(eval))
	var out KZGCommitment
	copy(out[:], bls.ToCompressedG1(g1))
	return out
}

// BytesToBLSField implements bytes_to_bls_field from the EIP-4844 consensus spec:
// https://github.com/ethereum/consensus-specs/blob/dev/specs/eip4844/polynomial-commitments.md#bytes_to_bls_field
func BytesToBLSField(h [32]byte) *bls.Fr {
	// re-interpret as little-endian
	var b [32]byte = h
	for i := 0; i < 16; i++ {
		b[31-i], b[i] = b[i], b[31-i]
	}
	zB := new(big.Int).Mod(new(big.Int).SetBytes(b[:]), BLSModulus)
	out := new(bls.Fr)
	bigToFr(out, zB)
	return out
}

// ComputeAggregatedPolyAndcommitment implements compute_aggregated_poly_and_commitment from the EIP-4844 consensus spec:
// https://github.com/ethereum/consensus-specs/blob/dev/specs/eip4844/polynomial-commitments.md#compute_aggregated_poly_and_commitment
func ComputeAggregatedPolyAndCommitment(blobs Polynomials, commitments KZGCommitmentSequence) ([]bls.Fr, *bls.G1Point, *bls.Fr, error) {
	// create challenges
	r, err := HashToBLSField(blobs, commitments)
	if err != nil {
		return nil, nil, nil, err
	}

	powers := ComputePowers(r, len(blobs))
	if len(powers) == 0 {
		return nil, nil, nil, errors.New("powers can't be 0 length")
	}

	var evaluationChallenge bls.Fr
	bls.MulModFr(&evaluationChallenge, r, &powers[len(powers)-1])

	aggregatedPoly, err := bls.PolyLinComb(blobs, powers)
	if err != nil {
		return nil, nil, nil, err
	}

	l := commitments.Len()
	commitmentsG1 := make([]bls.G1Point, l)
	for i := 0; i < l; i++ {
		c := commitments.At(i)
		p, err := bls.FromCompressedG1(c[:])
		if err != nil {
			return nil, nil, nil, err
		}
		bls.CopyG1(&commitmentsG1[i], p)
	}
	aggregatedCommitmentG1 := bls.LinCombG1(commitmentsG1, powers)
	return aggregatedPoly, aggregatedCommitmentG1, &evaluationChallenge, nil
}

// ComputeAggregateKZGProofFromPolynomials implements compute_aggregate_kzg_proof from the EIP-4844
// consensus spec, only operating over blobs that are already parsed into a polynomial.
func ComputeAggregateKZGProofFromPolynomials(blobs Polynomials) (KZGProof, error) {
	commitments := make(KZGCommitmentSequenceImpl, len(blobs))
	for i, b := range blobs {
		commitments[i] = PolynomialToKZGCommitment(Polynomial(b))
	}
	aggregatedPoly, _, evaluationChallenge, err := ComputeAggregatedPolyAndCommitment(blobs, commitments)
	if err != nil {
		return KZGProof{}, err
	}
	return ComputeKZGProof(aggregatedPoly, evaluationChallenge)
}

// ComputeAggregateKZGProof implements compute_kzg_proof from the EIP-4844 consensus spec:
// https://github.com/ethereum/consensus-specs/blob/dev/specs/eip4844/polynomial-commitments.md#compute_kzg_proof
func ComputeKZGProof(polynomial []bls.Fr, z *bls.Fr) (KZGProof, error) {
	y := EvaluatePolynomialInEvaluationForm(polynomial, z)
	polynomialShifted := make([]bls.Fr, len(polynomial))
	for i := range polynomial {
		bls.SubModFr(&polynomialShifted[i], &polynomial[i], y)
	}
	denominatorPoly := make([]bls.Fr, len(polynomial))
	if len(polynomial) != len(DomainFr) {
		return KZGProof{}, errors.New("polynomial has invalid length")
	}
	for i := range polynomial {
		if bls.EqualFr(&DomainFr[i], z) {
			return KZGProof{}, errors.New("invalid z challenge")
		}
		bls.SubModFr(&denominatorPoly[i], &DomainFr[i], z)
	}
	quotientPolynomial := make([]bls.Fr, len(polynomial))
	for i := range polynomial {
		bls.DivModFr(&quotientPolynomial[i], &polynomialShifted[i], &denominatorPoly[i])
	}
	rG1 := bls.LinCombG1(kzgSetupLagrange, quotientPolynomial)
	var proof KZGProof
	copy(proof[:], bls.ToCompressedG1(rG1))
	return proof, nil
}

// EvaluatePolynomialInEvaluationForm implements evaluate_polynomial_in_evaluation_form from the EIP-4844 consensus spec:
// https://github.com/ethereum/consensus-specs/blob/dev/specs/eip4844/polynomial-commitments.md#evaluate_polynomial_in_evaluation_form
func EvaluatePolynomialInEvaluationForm(poly []bls.Fr, x *bls.Fr) *bls.Fr {
	var result bls.Fr
	bls.EvaluatePolyInEvaluationForm(&result, poly, x, DomainFr, 0)
	return &result
}

// HashToBLSField implements hash_to_bls_field from the EIP-4844 consensus specs:
// https://github.com/ethereum/consensus-specs/blob/dev/specs/eip4844/polynomial-commitments.md#hash_to_bls_field
func HashToBLSField(polys Polynomials, comms KZGCommitmentSequence) (*bls.Fr, error) {
	sha := sha256.New()

	_, err := sha.Write([]byte(FIAT_SHAMIR_PROTOCOL_DOMAIN))
	if err != nil {
		return nil, err
	}

	bytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(bytes, uint64(FieldElementsPerBlob))
	_, err = sha.Write(bytes)
	if err != nil {
		return nil, err
	}

	bytes = make([]byte, 8)
	binary.LittleEndian.PutUint64(bytes, uint64(len(polys)))
	_, err = sha.Write(bytes)
	if err != nil {
		return nil, err
	}

	for _, poly := range polys {
		for _, fe := range poly {
			b32 := bls.FrTo32(&fe)
			_, err := sha.Write(b32[:])
			if err != nil {
				return nil, err
			}
		}
	}
	l := comms.Len()
	for i := 0; i < l; i++ {
		c := comms.At(i)
		_, err := sha.Write(c[:])
		if err != nil {
			return nil, err
		}
	}
	var hash [32]byte
	copy(hash[:], sha.Sum(nil))
	return BytesToBLSField(hash), nil
}

func BlobToPolynomial(b Blob) (Polynomial, bool) {
	l := b.Len()
	frs := make(Polynomial, l)
	for i := 0; i < l; i++ {
		if !bls.FrFrom32(&frs[i], b.At(i)) {
			return []bls.Fr{}, false
		}
	}
	return frs, true
}

func BlobsToPolynomials(blobs BlobSequence) ([][]bls.Fr, bool) {
	l := blobs.Len()
	out := make(Polynomials, l)
	for i := 0; i < l; i++ {
		blob, ok := BlobToPolynomial(blobs.At(i))
		if !ok {
			return nil, false
		}
		out[i] = blob
	}
	return out, true
}

func frToBig(b *big.Int, val *bls.Fr) {
	//b.SetBytes((*kilicbls.Fr)(val).RedToBytes())
	// silly double conversion
	v := bls.FrTo32(val)
	for i := 0; i < 16; i++ {
		v[31-i], v[i] = v[i], v[31-i]
	}
	b.SetBytes(v[:])
}

func bigToFr(out *bls.Fr, in *big.Int) bool {
	var b [32]byte
	inb := in.Bytes()
	copy(b[32-len(inb):], inb)
	// again, we have to double convert as go-kzg only accepts little-endian
	for i := 0; i < 16; i++ {
		b[31-i], b[i] = b[i], b[31-i]
	}
	return bls.FrFrom32(out, b)
}
