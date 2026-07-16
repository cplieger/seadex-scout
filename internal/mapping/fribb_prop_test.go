package mapping

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

// TestParseFribb_decodeOracleProperty is the every-PR randomized complement to
// the byte-level FuzzParseFribb target: it generates STRUCTURED Fribb bodies
// (which arbitrary fuzz bytes almost never form) and checks parseFribb against
// the generator's own model. For any list of well-formed records, every record
// with a non-zero AniList ID survives in order with its type normalized and
// its ids intact; ids encoded as JSON numbers and as quoted numeric strings
// decode identically (the flexInt metamorphic pair); zero-id records are
// dropped; and blank imdb entries are trimmed away.
func TestParseFribb_decodeOracleProperty(t *testing.T) {
	log := discardLogger()
	rapid.Check(t, func(t *rapid.T) {
		ids := rapid.SliceOfNDistinct(rapid.IntRange(1, 1<<31-1), 1, 20, rapid.ID).Draw(t, "ids")
		typ := rapid.SampledFrom([]string{"tv", " Movie ", "OVA", "special", "", "MUSIC"}).Draw(t, "type")
		tvdb := rapid.IntRange(0, 1<<31-1).Draw(t, "tvdb")
		imdbTails := rapid.SliceOfN(rapid.IntRange(1, 9_999_999), 0, 5).Draw(t, "imdb_tails")
		quoteID := rapid.Bool().Draw(t, "quote_id")

		imdb := make([]any, 0, len(imdbTails)+1)
		wantIMDb := make([]string, 0, len(imdbTails))
		for _, n := range imdbTails {
			imdb = append(imdb, "  tt"+strconv.Itoa(n)+"  ") // padded: decoder must trim
			wantIMDb = append(wantIMDb, "tt"+strconv.Itoa(n))
		}
		imdb = append(imdb, "   ") // blank entry: decoder must drop it

		body := make([]map[string]any, 0, len(ids)+1)
		for _, id := range ids {
			var encodedID any = id
			if quoteID {
				encodedID = strconv.Itoa(id) // quoted numeric string form
			}
			body = append(body, map[string]any{
				"anilist_id": encodedID,
				"type":       typ,
				"tvdb_id":    tvdb,
				"imdb_id":    imdb,
			})
		}
		body = append(body, map[string]any{"anilist_id": 0, "type": "tv"}) // dropped
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal generated body: %v", err)
		}

		records, err := parseFribb(data, log)
		if err != nil {
			t.Fatalf("parseFribb(generated body) error: %v", err)
		}
		if len(records) != len(ids) {
			t.Fatalf("parseFribb kept %d records, want %d (zero-id row dropped, others kept)", len(records), len(ids))
		}
		wantType := strings.ToUpper(strings.TrimSpace(typ))
		for i, rec := range records {
			if rec.AniListID != ids[i] {
				t.Fatalf("record[%d].AniListID = %d, want %d (order preserved; number/string forms equal)", i, rec.AniListID, ids[i])
			}
			if rec.Type != wantType {
				t.Fatalf("record[%d].Type = %q, want %q", i, rec.Type, wantType)
			}
			if rec.TvdbID != tvdb {
				t.Fatalf("record[%d].TvdbID = %d, want %d", i, rec.TvdbID, tvdb)
			}
			if len(rec.IMDbIDs) != len(wantIMDb) {
				t.Fatalf("record[%d].IMDbIDs = %v, want %v (padded trimmed, blank dropped)", i, rec.IMDbIDs, wantIMDb)
			}
			for j := range wantIMDb {
				if rec.IMDbIDs[j] != wantIMDb[j] {
					t.Fatalf("record[%d].IMDbIDs[%d] = %q, want %q", i, j, rec.IMDbIDs[j], wantIMDb[j])
				}
			}
		}
	})
}
