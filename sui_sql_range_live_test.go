package portfolio

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestSuiHistoryRangeMatchesPointReadsLive reads a real processor index over a
// range and checks every sample against a single ReadAtTime inside it: the same
// sample, the same groups, the same error. Configure it with
// PORTFOLIO_SUI_RANGE_LIVE=protocol,sqlURL,processorVersion,owner,from,to (times
// in RFC 3339) and PORTFOLIO_SENTIO_API_KEY.
func TestSuiHistoryRangeMatchesPointReadsLive(t *testing.T) {
	spec := os.Getenv("PORTFOLIO_SUI_RANGE_LIVE")
	if spec == "" {
		t.Skip("set PORTFOLIO_SUI_RANGE_LIVE to compare a range read with point reads")
	}
	fields := strings.Split(spec, ",")
	if len(fields) != 6 {
		t.Fatal("PORTFOLIO_SUI_RANGE_LIVE needs protocol,sqlURL,processorVersion,owner,from,to")
	}
	owner, err := ParseSuiAddress(fields[3])
	if err != nil {
		t.Fatal(err)
	}
	from, e1 := time.Parse(time.RFC3339, fields[4])
	to, e2 := time.Parse(time.RFC3339, fields[5])
	if e1 != nil || e2 != nil {
		t.Fatal("invalid range")
	}
	config := SentioIndexerConfig{SQLURL: fields[1], ProcessorVersion: fields[2]}
	var index *suiHistoryIndex
	switch fields[0] {
	case "navi":
		reader, err := NewNaviHistoryReader(config)
		if err != nil {
			t.Fatal(err)
		}
		index = reader.suiHistoryIndex
	case "suilend":
		reader, err := NewSuilendHistoryReader(config)
		if err != nil {
			t.Fatal(err)
		}
		index = reader.suiHistoryIndex
	default:
		t.Fatalf("unsupported protocol %q", fields[0])
	}
	ctx := context.Background()
	started := time.Now()
	samples, err := index.ReadRange(ctx, owner, from, to)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("range: %d samples in %s", len(samples), time.Since(started))
	// The range read runs under the production client timeout; the comparison
	// reads only need to finish.
	index.api.httpClient.Timeout = 90 * time.Second
	if len(samples) == 0 {
		t.Fatal("no samples in range")
	}
	for i, sample := range samples {
		if i > 0 && sample.Start.Before(samples[i-1].Until) {
			t.Fatalf("sample %d overlaps its predecessor", i)
		}
		for _, at := range []time.Time{sample.Start, sample.Start.Add(sample.Until.Sub(sample.Start) / 2)} {
			if at.Before(from) || at.After(to) {
				continue
			}
			// A transport failure of the comparison read says nothing about the
			// range; only an answered read is compared.
			var point SuiProtocolPositions
			var pointErr error
			started := time.Now()
			for attempt := 0; attempt < 3; attempt++ {
				if point, pointErr = index.ReadAtTime(ctx, owner, at); pointErr == nil || !strings.Contains(pointErr.Error(), "request failed") {
					break
				}
			}
			elapsed := time.Since(started)
			ranged, rangeErr := sample.At(at)
			if (pointErr == nil) != (rangeErr == nil) || (pointErr != nil && pointErr.Error() != rangeErr.Error()) {
				t.Fatalf("%s: point error %v, range error %v", at, pointErr, rangeErr)
			}
			if pointErr == nil && !reflect.DeepEqual(point, ranged) {
				t.Fatalf("%s: range sample differs from the point read\npoint: %+v\nrange: %+v", at, point, ranged)
			}
			t.Logf("%s checkpoint %d groups %d error %v (point read %s)", at.Format(time.RFC3339), ranged.Checkpoint.Sequence, len(ranged.Groups), rangeErr, elapsed)
		}
	}
}
