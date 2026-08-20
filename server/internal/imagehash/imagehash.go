// Package imagehash reduces a picture to two 64-bit numbers that survive being
// resized, recompressed and re-saved, so that two files holding the same
// photograph can be recognised as holding the same photograph.
//
// It is pure arithmetic over a grey plane somebody else decoded, for the same
// reason internal/takeout is pure parsing: the interesting part is the
// thresholds, the thresholds are guesses, and a guess is only adjustable if it
// can be tested without ImageMagick and a 100GB archive in the loop.
//
// Two hashes, and both are wanted:
//
//   - The difference hash is a gradient. It records, for each of 72 adjacent
//     pairs of pixels, only which one was brighter — so a JPEG at quality 60
//     and the same photograph at quality 95 agree almost perfectly, and so does
//     a copy half the size. What it cannot see is absolute brightness, which
//     means it also finds two photographs of nothing in particular to be the
//     same nothing.
//   - The perceptual hash is the low frequencies of a discrete cosine
//     transform: the coarse structure of the image, thrown away above the
//     eighth harmonic. It is the one that notices two flat frames are flat
//     differently, and it is more easily upset by a crop than the first one is.
//
// Requiring both keeps each one's blind spot from becoming the archive's. See
// internal/merge for the thresholds and for what they were measured against.
package imagehash

import (
	"fmt"
	"math"
)

// Version is the algorithm, stamped on every signature stored. Changing
// anything in this file that moves a bit means changing this, which is what
// makes a stored signature either current or requeueable rather than merely
// old. See db.StaleSignatures.
const Version = 1

// SampleEdge is the square grey plane everything here is computed from, and it
// is the whole of this package's interface to the outside world: 32x32 bytes,
// one byte of luminance per pixel, row-major, no header.
//
// 32 because the DCT wants a power of two comfortably larger than the 8x8 block
// it keeps, and because at that size the decode is what costs — a 12-megapixel
// HEIC takes a few hundred milliseconds to get into memory and microseconds to
// squeeze into a thousand bytes. Asking for a smaller plane would save nothing
// measurable and would leave the difference hash resampling up.
const SampleEdge = 32

// SampleSize is the length of the byte slice Compute expects.
const SampleSize = SampleEdge * SampleEdge

// Hashes is one picture, twice.
type Hashes struct {
	Difference uint64
	Perceptual uint64
}

// Compute reduces a SampleEdge-square grey plane to both hashes.
func Compute(gray []byte) (Hashes, error) {
	if len(gray) != SampleSize {
		return Hashes{}, fmt.Errorf(
			"imagehash: want a %dx%d grey plane (%d bytes), got %d",
			SampleEdge, SampleEdge, SampleSize, len(gray))
	}
	return Hashes{Difference: difference(gray), Perceptual: perceptual(gray)}, nil
}

// Distance is how many of the 64 bits two hashes disagree about — the Hamming
// distance, and the only thing any of these numbers is ever used for.
func Distance(a, b uint64) int { return popcount(a ^ b) }

// popcount is written out rather than reached for through math/bits so the
// hot loop in the scan has nothing to inline around. (Go's math/bits.OnesCount64
// compiles to a single instruction on amd64; this is the same call by another
// name, kept local so this package imports nothing but math and fmt.)
func popcount(x uint64) int {
	// SWAR: sum bits pairwise, then in nibbles, then in bytes, then multiply to
	// accumulate every byte's count into the top one.
	const (
		m1 = 0x5555555555555555
		m2 = 0x3333333333333333
		m4 = 0x0f0f0f0f0f0f0f0f
		h1 = 0x0101010101010101
	)
	x -= (x >> 1) & m1
	x = (x & m2) + ((x >> 2) & m2)
	x = (x + (x >> 4)) & m4
	return int((x * h1) >> 56)
}

// dHashWidth and dHashHeight are the grid the difference hash is taken over.
// Nine columns and eight rows, because what is hashed is the 72 *comparisons*
// between horizontally adjacent pixels, and eight rows of eight comparisons is
// the 64 bits wanted.
const (
	dHashWidth  = 9
	dHashHeight = 8
)

// difference is the gradient hash: bit set where a pixel is brighter than the
// one to its right.
//
// The 9x8 grid is resampled here rather than asked for from the decoder,
// because the decode is the expensive part and one plane has to serve both
// hashes. Doing it in Go also means the resampling is ours: a test can pin what
// a given plane hashes to, which is not true of a filter chain whose
// implementation belongs to whichever ImageMagick the host has.
func difference(gray []byte) uint64 {
	small := resample(gray, SampleEdge, SampleEdge, dHashWidth, dHashHeight)

	var out uint64
	bit := 0
	for y := range dHashHeight {
		for x := range dHashWidth - 1 {
			if small[y*dHashWidth+x] > small[y*dHashWidth+x+1] {
				out |= 1 << uint(bit)
			}
			bit++
		}
	}
	return out
}

// resample scales a grey plane bilinearly. Small, exact, and deliberately not
// area-averaging: the input is already a downsample somebody else did well, and
// what is wanted from here is a reproducible reading of it rather than a second
// opinion about how to shrink an image.
func resample(src []byte, sw, sh, dw, dh int) []float64 {
	out := make([]float64, dw*dh)
	for dy := range dh {
		// Pixel centres, which is what keeps the sampled grid centred on the
		// source rather than biased towards its top-left corner.
		sy := (float64(dy)+0.5)*float64(sh)/float64(dh) - 0.5
		y0, fy := split(sy, sh)
		y1 := min(y0+1, sh-1)

		for dx := range dw {
			sx := (float64(dx)+0.5)*float64(sw)/float64(dw) - 0.5
			x0, fx := split(sx, sw)
			x1 := min(x0+1, sw-1)

			tl := float64(src[y0*sw+x0])
			tr := float64(src[y0*sw+x1])
			bl := float64(src[y1*sw+x0])
			br := float64(src[y1*sw+x1])

			top := tl + (tr-tl)*fx
			bottom := bl + (br-bl)*fx
			out[dy*dw+dx] = top + (bottom-top)*fy
		}
	}
	return out
}

// split separates a source coordinate into the pixel left of it and how far
// past that pixel it lies, clamped at both edges.
func split(v float64, n int) (int, float64) {
	if v <= 0 {
		return 0, 0
	}
	i := int(v)
	if i >= n-1 {
		return n - 1, 0
	}
	return i, v - float64(i)
}

// dctKeep is the corner of the transform the perceptual hash is taken from:
// the 8x8 block of lowest frequencies, which is the image's shape with every
// detail above the eighth harmonic discarded.
const dctKeep = 8

// perceptual is the DCT hash: bit set where a low-frequency coefficient is
// above the median of its block.
//
// The median rather than the mean, which is the difference between a hash and a
// hash that works. One very bright or very dark region drags a mean far enough
// that most coefficients fall on the same side of it and the hash collapses
// towards all-ones or all-zeroes; the median cannot be moved that way, and
// splits the block in half by construction.
//
// The DC term is excluded from both the median and the bits. It is the average
// brightness of the whole frame, it is an order of magnitude larger than
// anything else in the block, and it is precisely the thing that must not
// matter: a photograph and the same photograph a stop darker are the same
// photograph.
func perceptual(gray []byte) uint64 {
	coeffs := dct2D(gray)

	// The DC term sits at index 0 and is dropped, which leaves 63 coefficients
	// for 64 bits. The last bit is the DC's own slot, left clear: it carries no
	// information either way, and a hash of a fixed length is worth more than a
	// bit reclaimed.
	rest := make([]float64, 0, dctKeep*dctKeep-1)
	for i, c := range coeffs {
		if i == 0 {
			continue
		}
		rest = append(rest, c)
	}
	mid := median(rest)

	var out uint64
	bit := 0
	for i := range coeffs {
		if i == 0 {
			continue
		}
		if coeffs[i] > mid {
			out |= 1 << uint(bit)
		}
		bit++
	}
	return out
}

// dct2D returns the top-left dctKeep x dctKeep coefficients of the type-II DCT
// of the sample plane, row-major.
//
// Separable, and only the wanted frequencies are ever evaluated: the rows are
// transformed into dctKeep columns first, and the columns of that into dctKeep
// rows. Roughly ten thousand multiply-adds for a plane this size, which is
// several hundred times cheaper than decoding the file it came from and not
// worth a library.
func dct2D(gray []byte) []float64 {
	cos := cosTable()

	// Rows: 32 of them, each reduced to dctKeep coefficients.
	rows := make([]float64, SampleEdge*dctKeep)
	for y := range SampleEdge {
		for u := range dctKeep {
			var sum float64
			for x := range SampleEdge {
				sum += float64(gray[y*SampleEdge+x]) * cos[u*SampleEdge+x]
			}
			rows[y*dctKeep+u] = sum * alpha(u)
		}
	}

	// Columns of that, giving the block.
	out := make([]float64, dctKeep*dctKeep)
	for u := range dctKeep {
		for v := range dctKeep {
			var sum float64
			for y := range SampleEdge {
				sum += rows[y*dctKeep+u] * cos[v*SampleEdge+y]
			}
			out[v*dctKeep+u] = sum * alpha(v)
		}
	}
	return out
}

// cosTable is cos((2x+1)uπ/2N) for every frequency the block keeps and every
// position in the plane. Built per call: it is 256 cosines against the ten
// thousand multiplies that follow, and a package-level table would be shared
// mutable state for no measurable gain.
func cosTable() []float64 {
	table := make([]float64, dctKeep*SampleEdge)
	for u := range dctKeep {
		for x := range SampleEdge {
			table[u*SampleEdge+x] = math.Cos(float64(2*x+1) * float64(u) * math.Pi / (2 * SampleEdge))
		}
	}
	return table
}

// alpha is the type-II DCT's orthonormal scale factor. It changes nothing about
// which coefficients are above the median — every term in a row is scaled by
// the same number — and it is here so the coefficients are a DCT rather than
// something proportional to one, which is what a test can be written against.
func alpha(u int) float64 {
	if u == 0 {
		return math.Sqrt(1 / float64(SampleEdge))
	}
	return math.Sqrt(2 / float64(SampleEdge))
}

// median of a slice it is allowed to reorder. The caller's slice is a local
// copy in both call sites.
func median(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sortFloats(values)
	half := len(values) / 2
	if len(values)%2 == 1 {
		return values[half]
	}
	return (values[half-1] + values[half]) / 2
}

// sortFloats is an insertion sort over 63 elements. slices.Sort would do, and
// this is here to keep the package's imports to math and fmt — see the doc
// comment: everything in it has to be arithmetic somebody can follow.
func sortFloats(v []float64) {
	for i := 1; i < len(v); i++ {
		x := v[i]
		j := i - 1
		for j >= 0 && v[j] > x {
			v[j+1] = v[j]
			j--
		}
		v[j+1] = x
	}
}
