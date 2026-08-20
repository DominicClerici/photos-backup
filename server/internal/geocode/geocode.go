// Package geocode turns a coordinate into the name of the nearest inhabited
// place, using a GeoNames extract held on disk and nothing else.
//
// Offline is the whole point. A reverse-geocoding API would mean the archive
// telling a third party where every photograph in it was taken, one request at
// a time, in exchange for a name — and it would mean "photos in Chicago"
// breaking when somebody else's service is down. The extract is 40MB of tab
// separated text, it resolves 11,000 coordinates in about a second, and the
// coordinates never leave the machine.
//
// It is also deliberately not machine learning. Nothing here needs a GPU, a
// model, or a decision about which model; place search works on the day the
// files are downloaded, months before anything can say what a photograph is of.
//
// # The data
//
// Three files from https://download.geonames.org/export/dump/, all of them
// required:
//
//	cities500.txt (or cities500.zip)   every populated place of 500 or more
//	admin1CodesASCII.txt               "US.IL" -> "Illinois"
//	countryInfo.txt                    "US" -> "United States"
//
// All three or none. Falling back to raw codes when one is missing would put
// "IL" in some rows and "Illinois" in others depending on what an operator
// happened to download, and the column those go into is search text — an
// inconsistency there is invisible until a query silently misses half the
// library.
package geocode

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sync"
)

// Source is what goes into assets.place_source: which thing resolved the name.
// It is recorded so that a later source — a better extract, a hand-typed "the
// cabin" — is distinguishable from this one without re-running anything.
const Source = "geonames"

// ErrNoData means the extract is not installed. It is a distinct error because
// it is the ordinary state of a fresh checkout rather than a fault: the caller
// should say what to download and carry on, not fail.
var ErrNoData = errors.New("geocode: no GeoNames extract installed")

const (
	citiesFile  = "cities500.txt"
	citiesZip   = "cities500.zip"
	admin1File  = "admin1CodesASCII.txt"
	countryFile = "countryInfo.txt"
)

// DownloadHint is what a command tells somebody who has not installed the
// extract yet. It lives here because this package is the one that knows which
// files it wants and under what names.
const DownloadHint = `download these into that directory from https://download.geonames.org/export/dump/:

    cities500.zip (or cities500.txt)   every populated place of 500 or more
    admin1CodesASCII.txt               state and province names
    countryInfo.txt                    country names`

// MaxDistanceKM is how far the nearest inhabited place may be before the answer
// stops being worth recording.
//
// A photograph taken over the Pacific is not "in Honolulu" because Honolulu is
// the closest thing to it with a name, and labelling it so would be worse than
// leaving the field empty: the empty field is honest, and the label would put
// the photograph into a search result for a city nobody was near. 150km is
// generous by land — almost nowhere populated is that far from a place of 500
// people — and short enough to fall silent over open water.
const MaxDistanceKM = 150

// A place covers the ground around it, and the coverage is what decides the
// answer — not which centre happens to be nearest.
//
// GeoNames gives every place one coordinate, its centre, and records a
// metropolis and the neighbourhoods, wards and villages inside and beside it as
// separate points. Nearest-centre therefore answers a photograph taken at the
// Eiffel Tower with "Paris 16 Passy" and one taken in Shibuya with the name of
// a block of 1,800 people, because in a dense city the nearest centre is never
// the city's own. Both are correct and both are useless: the word somebody will
// type is Paris, and Tokyo.
//
// So each place is given a radius from its population, on the assumption of
// coverDensity people to the square kilometre, and the largest place whose
// radius reaches the photograph wins. Chicago comes out at 13.6km, which is
// close to the real thing; a hamlet of 500 comes out at 200 metres and covers
// nothing but itself.
//
// The trade this makes is worth naming, because it is visible in the results.
// A photograph taken in Oak Park, which shares a border with Chicago, reads as
// Chicago. One taken in Evanston, 18km up the lakefront and outside the radius,
// stays Evanston. The line falls where a city's own extent falls, which is the
// most defensible place for it, and the direction of the error is the one that
// helps: a search box is a place to type the name of a city, not the name of a
// ward.
//
// maxCoverKM stops that reasoning where it breaks down. A municipality of
// twenty million would otherwise claim everything within 45km, which is no
// longer a city so much as a region.
const (
	coverDensity = 4000.0
	maxCoverKM   = 30.0
)

// Place is one inhabited place and how far the query was from it.
type Place struct {
	City       string
	Admin1     string
	Country    string
	Lat, Lon   float64
	Population int
	DistanceKM float64
}

// earthRadiusKM is the mean radius. The error against a real ellipsoid is a few
// kilometres at worst, which does not matter to any question this answers:
// nothing here reports a distance to a user, and picking between two cities is
// not a decision a 0.3% scale error can change.
const earthRadiusKM = 6371.0088

type place struct {
	name    string
	admin1  string
	country string
	lat     float32
	lon     float32
	pop     int32
	// cover2 is the square of the radius above, in square kilometres, so the
	// test against it is the same squared distance the tree already computes.
	cover2 float32
}

// vec3 is a place on the unit sphere times the earth's radius, in kilometres.
//
// Cartesian rather than latitude and longitude, because the tree below needs a
// metric it can split on. Degrees are not one: a degree of longitude is 111km
// at the equator and 40km in Alaska, the antimeridian is a discontinuity in the
// middle of the Pacific, and every meridian meets at the poles. In three
// dimensions all of that disappears, and straight-line distance through the
// earth is monotonic in distance across it — so the nearest by chord is the
// nearest by any measure anyone means. Under 30km the two agree to within a
// centimetre, which is why cover2 can be compared against a chord directly.
type vec3 struct{ x, y, z float32 }

type node struct {
	point       int32
	left, right int32
	axis        uint8
}

// Index is a loaded extract: the places, and a k-d tree over them.
type Index struct {
	places []place
	coords []vec3
	nodes  []node
	root   int32
}

// Loader defers Load until something actually asks for a place, and remembers
// the answer — including a failure.
//
// Lazy because the common startup has nothing to geocode: a server that has
// already been through its library pays 40MB of parsing and 65MB of memory for
// a question nobody is going to ask until the next photograph with a GPS fix
// arrives. Cached because the failure is a missing file, and retrying a missing
// file once per asset through an 11,000-row backfill would be 11,000 identical
// disappointments.
type Loader struct {
	dir  string
	once sync.Once
	ix   *Index
	err  error
}

func NewLoader(dir string) *Loader { return &Loader{dir: dir} }

func (l *Loader) Dir() string { return l.dir }

func (l *Loader) Index() (*Index, error) {
	l.once.Do(func() {
		if l.dir == "" {
			l.err = fmt.Errorf("%w: no directory configured", ErrNoData)
			return
		}
		l.ix, l.err = Load(l.dir)
	})
	return l.ix, l.err
}

// Load reads the extract in dir and builds the tree.
func Load(dir string) (*Index, error) {
	admin1, err := loadAdmin1(filepath.Join(dir, admin1File))
	if err != nil {
		return nil, err
	}
	countries, err := loadCountries(filepath.Join(dir, countryFile))
	if err != nil {
		return nil, err
	}

	ix := &Index{}
	if err := ix.loadCities(dir, admin1, countries); err != nil {
		return nil, err
	}
	if len(ix.places) == 0 {
		return nil, fmt.Errorf("%s holds no populated places", filepath.Join(dir, citiesFile))
	}

	idx := make([]int32, len(ix.places))
	for i := range idx {
		idx[i] = int32(i)
	}
	ix.nodes = make([]node, 0, len(idx))
	ix.root = ix.build(idx)
	return ix, nil
}

// Len is how many places were loaded.
func (ix *Index) Len() int { return len(ix.places) }

// coverRadiusKM is how far a place of this size reaches. See coverDensity.
func coverRadiusKM(pop int32) float64 {
	if pop <= 0 {
		return 0
	}
	r := math.Sqrt(float64(pop) / (coverDensity * math.Pi))
	return min(r, maxCoverKM)
}

// Nearest names the inhabited place a coordinate is in: the largest place whose
// reach covers it, or failing that the closest one within MaxDistanceKM.
func (ix *Index) Nearest(lat, lon float64) (Place, bool) {
	if ix == nil || len(ix.places) == 0 {
		return Place{}, false
	}
	if lat < -90 || lat > 90 || lon < -180 || lon > 180 {
		return Place{}, false
	}

	s := scan{q: toVec(lat, lon), nearest: -1, covering: -1, nearestDist2: math.MaxFloat32}
	ix.search(ix.root, &s)

	// The covering place first, and the nearest only when nothing covers —
	// which is the ordinary case in the countryside, where the answer is the
	// market town some distance away rather than nothing at all.
	point, dist2 := s.covering, s.coveringDist2
	if point < 0 {
		point, dist2 = s.nearest, s.nearestDist2
	}
	if point < 0 {
		return Place{}, false
	}

	km := surfaceKM(math.Sqrt(float64(dist2)))
	if km > MaxDistanceKM {
		return Place{}, false
	}
	p := ix.places[point]
	return Place{
		City:       p.name,
		Admin1:     p.admin1,
		Country:    p.country,
		Lat:        float64(p.lat),
		Lon:        float64(p.lon),
		Population: int(p.pop),
		DistanceKM: km,
	}, true
}

// scan is one query in flight: the closest place seen so far, and the largest
// one whose reach covers the point.
type scan struct {
	q             vec3
	nearest       int32
	nearestDist2  float32
	covering      int32
	coveringDist2 float32
	coveringPop   int32
}

func (ix *Index) search(at int32, s *scan) {
	if at < 0 {
		return
	}
	n := ix.nodes[at]
	c := ix.coords[n.point]
	d2 := dist2(s.q, c)

	if d2 < s.nearestDist2 {
		s.nearest, s.nearestDist2 = n.point, d2
	}
	if p := &ix.places[n.point]; d2 <= p.cover2 && p.pop > s.coveringPop {
		s.covering, s.coveringDist2, s.coveringPop = n.point, d2, p.pop
	}

	// Down the side the query is on first, so the far side is compared against
	// a distance that is already small.
	delta := axis(s.q, n.axis) - axis(c, n.axis)
	near, far := n.left, n.right
	if delta > 0 {
		near, far = n.right, n.left
	}
	ix.search(near, s)

	// The far side is worth entering if it could hold something closer, or
	// anything at all that might cover the point — which is why this is not
	// simply the 1-nearest test. A city big enough to reach the query can be
	// a long way past the nearest hamlet, and pruning on the hamlet's distance
	// would be how it gets missed.
	if limit := max(s.nearestDist2, maxCoverKM*maxCoverKM); delta*delta < limit {
		ix.search(far, s)
	}
}

// build partitions idx into a balanced tree and returns the node index of its
// root, splitting on whichever axis the points are most spread along.
//
// Widest-axis rather than round-robin, because the places are not spread evenly
// through the cube: they sit on the surface of a sphere and cluster hard on the
// land parts of it. Splitting a slab that is wide in x and flat in z along z
// buys almost no separation and costs a level of depth.
func (ix *Index) build(idx []int32) int32 {
	if len(idx) == 0 {
		return -1
	}
	ax := ix.widestAxis(idx)
	mid := len(idx) / 2
	ix.selectNth(idx, mid, ax)

	at := int32(len(ix.nodes))
	ix.nodes = append(ix.nodes, node{point: idx[mid], axis: ax})
	left := ix.build(idx[:mid])
	right := ix.build(idx[mid+1:])
	ix.nodes[at].left, ix.nodes[at].right = left, right
	return at
}

func (ix *Index) widestAxis(idx []int32) uint8 {
	var lo, hi [3]float32
	for a := range lo {
		lo[a], hi[a] = math.MaxFloat32, -math.MaxFloat32
	}
	for _, i := range idx {
		c := ix.coords[i]
		for a := uint8(0); a < 3; a++ {
			v := axis(c, a)
			if v < lo[a] {
				lo[a] = v
			}
			if v > hi[a] {
				hi[a] = v
			}
		}
	}
	best, spread := uint8(0), hi[0]-lo[0]
	for a := uint8(1); a < 3; a++ {
		if s := hi[a] - lo[a]; s > spread {
			best, spread = a, s
		}
	}
	return best
}

// selectNth partially sorts idx so that idx[k] holds the element that would be
// there if the whole slice were sorted on ax, and everything before it is no
// greater. Quickselect, because a full sort at every level of a 225,000-point
// tree is the difference between building it in half a second and building it
// in five.
func (ix *Index) selectNth(idx []int32, k int, ax uint8) {
	lo, hi := 0, len(idx)-1
	for lo < hi {
		p := ix.partition(idx, lo, hi, ax)
		switch {
		case k == p:
			return
		case k < p:
			hi = p - 1
		default:
			lo = p + 1
		}
	}
}

func (ix *Index) partition(idx []int32, lo, hi int, ax uint8) int {
	// Median of three, which is what keeps already-ordered input — a file
	// sorted by anything correlated with position — off the quadratic path.
	mid := lo + (hi-lo)/2
	if ix.less(idx[mid], idx[lo], ax) {
		idx[lo], idx[mid] = idx[mid], idx[lo]
	}
	if ix.less(idx[hi], idx[lo], ax) {
		idx[lo], idx[hi] = idx[hi], idx[lo]
	}
	if ix.less(idx[hi], idx[mid], ax) {
		idx[mid], idx[hi] = idx[hi], idx[mid]
	}
	pivot := axis(ix.coords[idx[mid]], ax)
	idx[mid], idx[hi] = idx[hi], idx[mid]

	store := lo
	for i := lo; i < hi; i++ {
		if axis(ix.coords[idx[i]], ax) < pivot {
			idx[store], idx[i] = idx[i], idx[store]
			store++
		}
	}
	idx[store], idx[hi] = idx[hi], idx[store]
	return store
}

func (ix *Index) less(a, b int32, ax uint8) bool {
	return axis(ix.coords[a], ax) < axis(ix.coords[b], ax)
}

func axis(v vec3, a uint8) float32 {
	switch a {
	case 0:
		return v.x
	case 1:
		return v.y
	default:
		return v.z
	}
}

func dist2(a, b vec3) float32 {
	dx, dy, dz := a.x-b.x, a.y-b.y, a.z-b.z
	return dx*dx + dy*dy + dz*dz
}

func toVec(lat, lon float64) vec3 {
	rlat, rlon := lat*math.Pi/180, lon*math.Pi/180
	cos := math.Cos(rlat)
	return vec3{
		x: float32(earthRadiusKM * cos * math.Cos(rlon)),
		y: float32(earthRadiusKM * cos * math.Sin(rlon)),
		z: float32(earthRadiusKM * math.Sin(rlat)),
	}
}

// surfaceKM converts a straight line through the earth into a distance across
// it. Below a few hundred kilometres the two are within a metre of each other;
// the conversion is here so that MaxDistanceKM means what it says.
func surfaceKM(chord float64) float64 {
	half := chord / (2 * earthRadiusKM)
	if half > 1 {
		half = 1
	}
	return 2 * earthRadiusKM * math.Asin(half)
}
