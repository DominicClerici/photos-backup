package geocode

import (
	"archive/zip"
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// GeoNames column numbers, zero-based, from the layout documented at
// https://download.geonames.org/export/dump/readme.txt. Named because
// `f[10]` in the middle of a parse loop is unreadable and, worse, unverifiable.
const (
	colName       = 1
	colLatitude   = 4
	colLongitude  = 5
	colClass      = 6
	colCode       = 7
	colCountry    = 8
	colAdmin1     = 10
	colPopulation = 14
	cityColumns   = 19
)

// skipped are the feature codes in cities500 that are not somewhere a
// photograph can be taken today.
//
// PPLX is the important one: a section of a populated place, which is to say a
// neighbourhood. Its coordinate sits inside a city and is therefore nearly
// always closer to a photograph than the city's own centre, so leaving these in
// means downtown Chicago geocodes to "Loop" and the search for the city finds
// nothing. The rest are places that no longer exist — historical, abandoned,
// destroyed — and a photograph from last summer was not taken in one.
var skipped = map[string]bool{
	"PPLX":  true,
	"PPLH":  true,
	"PPLQ":  true,
	"PPLW":  true,
	"PPLCH": true,
}

func (ix *Index) loadCities(dir string, admin1, countries map[string]string) error {
	txt := filepath.Join(dir, citiesFile)
	if f, err := os.Open(txt); err == nil {
		defer f.Close()
		return ix.readCities(bufio.NewReaderSize(f, 1<<20), admin1, countries)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("open %s: %w", txt, err)
	}

	// The zip as downloaded, so that installing the extract is three curls and
	// no unzip. It holds cities500.txt and a readme.
	archive := filepath.Join(dir, citiesZip)
	zr, err := zip.OpenReader(archive)
	if os.IsNotExist(err) {
		return fmt.Errorf("%w: %s holds neither %s nor %s", ErrNoData, dir, citiesFile, citiesZip)
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", archive, err)
	}
	defer zr.Close()

	for _, entry := range zr.File {
		if entry.Name != citiesFile {
			continue
		}
		rc, err := entry.Open()
		if err != nil {
			return fmt.Errorf("read %s from %s: %w", citiesFile, archive, err)
		}
		defer rc.Close()
		return ix.readCities(bufio.NewReaderSize(rc, 1<<20), admin1, countries)
	}
	return fmt.Errorf("%s does not contain %s", archive, citiesFile)
}

func (ix *Index) readCities(r io.Reader, admin1, countries map[string]string) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)

	var f [cityColumns]string
	for line := 1; sc.Scan(); line++ {
		text := sc.Text()
		if text == "" {
			continue
		}
		if n := splitTabs(text, f[:]); n < cityColumns {
			return fmt.Errorf("%s line %d: %d columns, want %d", citiesFile, line, n, cityColumns)
		}
		if f[colClass] != "P" || skipped[f[colCode]] {
			continue
		}

		lat, err := strconv.ParseFloat(f[colLatitude], 64)
		if err != nil {
			return fmt.Errorf("%s line %d: latitude %q: %w", citiesFile, line, f[colLatitude], err)
		}
		lon, err := strconv.ParseFloat(f[colLongitude], 64)
		if err != nil {
			return fmt.Errorf("%s line %d: longitude %q: %w", citiesFile, line, f[colLongitude], err)
		}
		// Population is blank on a few thousand rows and zero on more. Neither
		// is a parse failure; both mean "no idea how big this is", which the
		// preference rule reads as "small".
		pop, _ := strconv.Atoi(f[colPopulation])

		cover := coverRadiusKM(int32(pop))
		ix.places = append(ix.places, place{
			name:    f[colName],
			admin1:  admin1[f[colCountry]+"."+f[colAdmin1]],
			country: countries[f[colCountry]],
			lat:     float32(lat),
			lon:     float32(lon),
			pop:     int32(pop),
			cover2:  float32(cover * cover),
		})
		ix.coords = append(ix.coords, toVec(lat, lon))
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read %s: %w", citiesFile, err)
	}
	return nil
}

// loadAdmin1 maps "US.IL" to "Illinois".
func loadAdmin1(path string) (map[string]string, error) {
	names := make(map[string]string, 4096)
	err := eachLine(path, func(line string) error {
		var f [4]string
		if n := splitTabs(line, f[:]); n < 2 {
			return nil
		}
		names[f[0]] = f[1]
		return nil
	})
	return names, err
}

// loadCountries maps "US" to "United States".
func loadCountries(path string) (map[string]string, error) {
	const (
		colISO  = 0
		colName = 4
	)
	names := make(map[string]string, 256)
	err := eachLine(path, func(line string) error {
		// The file leads with about fifty lines of commented documentation.
		if strings.HasPrefix(line, "#") {
			return nil
		}
		var f [colName + 1]string
		if n := splitTabs(line, f[:]); n < colName+1 {
			return nil
		}
		names[f[colISO]] = f[colName]
		return nil
	})
	return names, err
}

func eachLine(path string, fn func(string) error) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return fmt.Errorf("%w: %s is missing", ErrNoData, path)
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			if err := fn(line); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	return nil
}

// splitTabs fills dst with the leading tab-separated fields of line and returns
// how many it wrote, capped at len(dst).
//
// Hand-rolled rather than strings.Split because this runs 235,000 times over a
// file with nineteen columns, and Split would allocate a slice of nineteen
// strings for every one of them to read six.
func splitTabs(line string, dst []string) int {
	n := 0
	for n < len(dst) {
		i := strings.IndexByte(line, '\t')
		if i < 0 {
			dst[n] = line
			return n + 1
		}
		dst[n] = line[:i]
		line = line[i+1:]
		n++
	}
	return n
}
