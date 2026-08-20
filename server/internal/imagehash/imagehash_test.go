package imagehash

import (
	"math"
	"math/rand/v2"
	"testing"
)

// plane builds a sample from a function of position, which is how every
// synthetic picture below is written.
func plane(f func(x, y int) byte) []byte {
	out := make([]byte, SampleSize)
	for y := range SampleEdge {
		for x := range SampleEdge {
			out[y*SampleEdge+x] = f(x, y)
		}
	}
	return out
}

// gradient is the picture most of these tests are about: brightness rising
// left to right, which every difference-hash bit has an opinion about.
func gradient() []byte {
	return plane(func(x, _ int) byte { return byte(x * 8) })
}

func TestComputeRejectsTheWrongSize(t *testing.T) {
	for _, n := range []int{0, SampleSize - 1, SampleSize + 1} {
		if _, err := Compute(make([]byte, n)); err == nil {
			t.Errorf("Compute accepted a %d-byte plane; want an error", n)
		}
	}
}

// A flat image has no gradient anywhere, so every comparison is "not brighter"
// and the difference hash is zero. Worth pinning because it is also the shape
// of the failure this package's second hash exists to catch.
func TestDifferenceOfAFlatImageIsZero(t *testing.T) {
	for _, level := range []byte{0, 128, 255} {
		h, err := Compute(plane(func(int, int) byte { return level }))
		if err != nil {
			t.Fatal(err)
		}
		if h.Difference != 0 {
			t.Errorf("flat plane at %d: difference = %#016x, want 0", level, h.Difference)
		}
	}
}

// The other half of that: two flat images at different levels are identical to
// the difference hash and must not be identical overall. This is the whole
// argument for storing two numbers.
func TestFlatImagesAtDifferentLevelsShareADifferenceHash(t *testing.T) {
	dark, err := Compute(plane(func(int, int) byte { return 20 }))
	if err != nil {
		t.Fatal(err)
	}
	light, err := Compute(plane(func(int, int) byte { return 200 }))
	if err != nil {
		t.Fatal(err)
	}
	if dark.Difference != light.Difference {
		t.Fatalf("expected two flat planes to agree on the difference hash; got %#016x and %#016x",
			dark.Difference, light.Difference)
	}
}

// A rising ramp is brighter to the right everywhere, so every one of the 64
// comparisons is false and the hash is zero; mirrored, every one is true.
func TestDifferenceReadsDirection(t *testing.T) {
	rising, err := Compute(gradient())
	if err != nil {
		t.Fatal(err)
	}
	falling, err := Compute(plane(func(x, _ int) byte { return byte((SampleEdge - 1 - x) * 8) }))
	if err != nil {
		t.Fatal(err)
	}

	if rising.Difference != 0 {
		t.Errorf("left-to-right ramp: difference = %#016x, want 0", rising.Difference)
	}
	if want := uint64(math.MaxUint64); falling.Difference != want {
		t.Errorf("right-to-left ramp: difference = %#016x, want %#016x", falling.Difference, want)
	}
	if d := Distance(rising.Difference, falling.Difference); d != 64 {
		t.Errorf("a ramp and its mirror are %d bits apart, want 64", d)
	}
}

// Brightness is what the perceptual hash is built to ignore: the DC term is
// excluded, so the same picture a stop darker is bit-for-bit the same hash.
func TestPerceptualIgnoresBrightness(t *testing.T) {
	base, err := Compute(plane(func(x, y int) byte { return byte((x*3 + y*5) % 200) }))
	if err != nil {
		t.Fatal(err)
	}
	// Halved, which moves every coefficient including the DC and moves the
	// median with them. Nothing about which coefficients are above their own
	// median changes.
	darker, err := Compute(plane(func(x, y int) byte { return byte(((x*3 + y*5) % 200) / 2) }))
	if err != nil {
		t.Fatal(err)
	}
	if base.Perceptual != darker.Perceptual {
		t.Errorf("halving every pixel moved the perceptual hash %d bits",
			Distance(base.Perceptual, darker.Perceptual))
	}
}

// The DC term's slot is left clear rather than reused, so the top bit of every
// perceptual hash is always zero. Pinned because the alternative — packing 64
// coefficients by including the DC — is the obvious "fix" somebody would apply
// to the gap, and it would silently make brightness matter again.
func TestPerceptualLeavesTheTopBitClear(t *testing.T) {
	for i := range 20 {
		h, err := Compute(plane(func(x, y int) byte { return byte((x*i + y*(i+3)) % 256) }))
		if err != nil {
			t.Fatal(err)
		}
		if h.Perceptual&(1<<63) != 0 {
			t.Fatalf("sample %d set the reserved top bit: %#016x", i, h.Perceptual)
		}
	}
}

// jitter is what a lossy round trip does to a plane: every pixel nudged,
// nothing moved. Deterministic, so a failure is reproducible rather than a
// thing that happens on some runs.
func jitter(src []byte, seed uint64, amplitude int) []byte {
	rng := rand.New(rand.NewPCG(seed, seed*2+1))
	out := make([]byte, len(src))
	for i, v := range src {
		out[i] = byte(min(max(int(v)+rng.IntN(2*amplitude+1)-amplitude, 0), 255))
	}
	return out
}

// The property the whole feature rests on: a picture that has been through a
// re-encode is still recognisably itself.
//
// The plane is busy on purpose — detail at several scales, which is what a
// photograph has and what puts energy across the DCT block being hashed. See
// the test below for what happens when it does not.
func TestNoiseBarelyMovesEitherHash(t *testing.T) {
	src := plane(func(x, y int) byte {
		fx, fy := float64(x), float64(y)
		return byte(128 +
			50*math.Sin(fx/4) +
			40*math.Cos(fy/3) +
			30*math.Sin((fx+fy)/1.7) +
			20*math.Cos(fx*fy/9))
	})
	clean, err := Compute(src)
	if err != nil {
		t.Fatal(err)
	}
	dirty, err := Compute(jitter(src, 1, 8))
	if err != nil {
		t.Fatal(err)
	}

	if d := Distance(clean.Difference, dirty.Difference); d > 8 {
		t.Errorf("±8 of noise moved the difference hash %d bits; want at most 8", d)
	}
	if d := Distance(clean.Perceptual, dirty.Perceptual); d > 8 {
		t.Errorf("±8 of noise moved the perceptual hash %d bits; want at most 8", d)
	}
}

// The perceptual hash's own weakness, pinned rather than papered over, because
// the thresholds in internal/merge are chosen around it.
//
// A picture with almost nothing in it — a smooth gradient, a wall, an
// underexposed frame — puts nearly all of its energy in two or three
// coefficients and leaves the other sixty hovering around zero. Those sixty are
// then decided by whatever noise is present, so the same picture re-encoded
// twice can differ by twenty bits or more. The difference hash is untroubled by
// the same plane, which is the division of labour this package is built on.
//
// The consequence downstream is a false *negative*: two copies of a nearly
// blank photograph may not be offered as duplicates. That is the right way for
// this to fail. The alternative reading — trusting the difference hash alone —
// makes every blank frame in the archive a duplicate of every other one.
func TestPerceptualIsUnstableOnALowDetailPicture(t *testing.T) {
	src := plane(func(x, y int) byte {
		return byte(128 + 60*math.Sin(float64(x)/4) + 60*math.Cos(float64(y)/3))
	})
	clean, err := Compute(src)
	if err != nil {
		t.Fatal(err)
	}
	dirty, err := Compute(jitter(src, 1, 8))
	if err != nil {
		t.Fatal(err)
	}

	if d := Distance(clean.Difference, dirty.Difference); d > 8 {
		t.Errorf("the difference hash is supposed to cope with this plane; it moved %d bits", d)
	}
	if d := Distance(clean.Perceptual, dirty.Perceptual); d <= 8 {
		t.Errorf("perceptual hash moved only %d bits on a low-detail plane. "+
			"That is better than documented — if it is reliably true, the guard in "+
			"internal/merge can be tightened and this test is the wrong shape", d)
	}
}

// And the property that makes the first one worth anything: two pictures that
// are not the same picture land far apart. A threshold is only a threshold if
// the two populations are separated.
func TestUnrelatedPicturesLandFarApart(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	first, err := Compute(plane(func(x, y int) byte {
		return byte(math.Sin(float64(x*y)/7)*120 + 128)
	}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Compute(plane(func(int, int) byte { return byte(rng.IntN(256)) }))
	if err != nil {
		t.Fatal(err)
	}

	if d := Distance(first.Perceptual, second.Perceptual); d < 16 {
		t.Errorf("two unrelated pictures are %d perceptual bits apart; want well clear of any threshold", d)
	}
}

func TestDistanceCountsBits(t *testing.T) {
	cases := []struct {
		a, b uint64
		want int
	}{
		{0, 0, 0},
		{0, 1, 1},
		{0, math.MaxUint64, 64},
		{0xff00ff00ff00ff00, 0x00ff00ff00ff00ff, 64},
		{0b1011, 0b1110, 2},
	}
	for _, c := range cases {
		if got := Distance(c.a, c.b); got != c.want {
			t.Errorf("Distance(%#x, %#x) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

// resample has to land the destination grid centred on the source rather than
// against its corner, or every hash is biased towards the top-left of the
// picture. A constant plane resamples to a constant one whatever the scale.
func TestResampleKeepsAConstantConstant(t *testing.T) {
	src := make([]byte, SampleSize)
	for i := range src {
		src[i] = 77
	}
	for _, size := range [][2]int{{9, 8}, {3, 3}, {SampleEdge, SampleEdge}, {40, 40}} {
		out := resample(src, SampleEdge, SampleEdge, size[0], size[1])
		for i, v := range out {
			if math.Abs(v-77) > 1e-9 {
				t.Fatalf("resample to %dx%d: pixel %d = %v, want 77", size[0], size[1], i, v)
			}
		}
	}
}

// A linear ramp resampled stays monotonic. Cheap, and it is the one way a
// sign error in split() shows up as something other than noise.
func TestResampleKeepsARampMonotonic(t *testing.T) {
	out := resample(gradient(), SampleEdge, SampleEdge, dHashWidth, dHashHeight)
	for y := range dHashHeight {
		for x := range dHashWidth - 1 {
			if out[y*dHashWidth+x] >= out[y*dHashWidth+x+1] {
				t.Fatalf("row %d is not rising at column %d: %v then %v",
					y, x, out[y*dHashWidth+x], out[y*dHashWidth+x+1])
			}
		}
	}
}

// The DC coefficient of a type-II DCT is the mean times the edge length. It is
// the one term with a closed form, so it is the one that says the transform is
// a DCT and not merely something shaped like one.
func TestDCTDCTermIsTheMean(t *testing.T) {
	const level = 100
	coeffs := dct2D(plane(func(int, int) byte { return level }))

	want := float64(level) * SampleEdge
	if math.Abs(coeffs[0]-want) > 1e-6 {
		t.Errorf("DC = %v, want %v", coeffs[0], want)
	}
	// A flat plane has no other frequencies in it at all.
	for i, c := range coeffs[1:] {
		if math.Abs(c) > 1e-6 {
			t.Errorf("coefficient %d of a flat plane = %v, want 0", i+1, c)
		}
	}
}

func TestMedian(t *testing.T) {
	cases := []struct {
		in   []float64
		want float64
	}{
		{nil, 0},
		{[]float64{5}, 5},
		{[]float64{3, 1, 2}, 2},
		{[]float64{4, 1, 3, 2}, 2.5},
		{[]float64{-1, -5, 9, 3}, 1},
	}
	for _, c := range cases {
		if got := median(append([]float64(nil), c.in...)); got != c.want {
			t.Errorf("median(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}
