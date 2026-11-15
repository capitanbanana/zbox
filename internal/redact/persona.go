// Package redact builds the fictional VM identity (legend), harvests host
// machine facts, and turns both into claude-code-redact rules.
package redact

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Legend is the fictional identity a VM presents to model providers. Every
// value is derived deterministically from the VM seed, so a restart of the
// same VM never changes replacements (prompt caches stay valid).
type Legend struct {
	Username    string `json:"username"`
	FullName    string `json:"full_name"`
	Email       string `json:"email"`
	GitHub      string `json:"github"`
	Hostname    string `json:"hostname"`
	Domain      string `json:"domain"`
	TimeZone    string `json:"timezone"`
	FirstStart  string `json:"first_start"`
	NumStartups int    `json:"num_startups"`
	MachineID   string `json:"machine_id"`
	UserID      string `json:"user_id"`
	AnonymousID string `json:"anonymous_id"`
}

// Home is the guest home directory implied by the legend.
func (l Legend) Home() string { return "/home/" + l.Username }

// DevPath is the legend-consistent location for an exported project.
func (l Legend) DevPath(name string) string { return l.Home() + "/dev/" + name }

// ClaudeJSON renders the ~/.claude.json bootstrap for a machine that has been
// in use for a while but never contacted Anthropic under the host identity.
func (l Legend) ClaudeJSON() []byte {
	doc := map[string]any{
		"numStartups":    l.NumStartups,
		"firstStartTime": l.FirstStart,
		"machineID":      l.MachineID,
		"userID":         l.UserID,
		"anonymousId":    l.AnonymousID,
		"oauthAccount":   map[string]any{},
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return []byte("{}\n")
	}
	return append(b, '\n')
}

// NewLegend derives a legend from a VM seed. now only anchors "born not
// today": the result is stored in config.json and never regenerated.
func NewLegend(seed []byte, now time.Time) Legend {
	first := pick(newStream(seed, "first-name"), firstNames)
	last := pick(newStream(seed, "last-name"), lastNames)
	title := func(s string) string { return strings.ToUpper(s[:1]) + s[1:] }
	initial := strings.ToLower(last[:1])

	userStream := newStream(seed, "username")
	username := fmt.Sprintf("%s%s%02d", first, initial, userStream.intn(90)+10)
	github := first + initial + fmt.Sprintf("%02d", newStream(seed, "github").intn(9000)+1000)

	hostname := pick(newStream(seed, "hostname-adj"), hostAdjectives) + "-" +
		pick(newStream(seed, "hostname-noun"), hostNouns)
	domain := pick(newStream(seed, "domain-word"), domainWords) + "." +
		pick(newStream(seed, "domain-tld"), domainTLDs)

	clock := newStream(seed, "first-start")
	firstStart := now.Add(-time.Duration(21+clock.intn(21)) * 24 * time.Hour)
	firstStart = time.Date(firstStart.Year(), firstStart.Month(), firstStart.Day(),
		8+clock.intn(12), clock.intn(60), clock.intn(60),
		clock.intn(1000)*int(time.Millisecond), time.UTC)

	idStream := newStream(seed, "ids")
	return Legend{
		Username:    username,
		FullName:    title(first) + " " + title(last),
		Email:       first + "." + last + "@" + pick(newStream(seed, "mail-domain"), mailDomains),
		GitHub:      github,
		Hostname:    hostname,
		Domain:      domain,
		TimeZone:    pick(newStream(seed, "timezone"), timeZones),
		FirstStart:  firstStart.Format("2006-01-02T15:04:05.000Z"),
		NumStartups: 5 + newStream(seed, "startups").intn(16),
		MachineID:   hex.EncodeToString(idStream.next(32)),
		UserID:      hex.EncodeToString(idStream.next(32)),
		AnonymousID: "claudecode.v1." + uuidv4(idStream.next(16)),
	}
}

// Replacement maps a harvested value of category onto a legend-consistent
// neutral value. Secrets (KEY/CUSTOM/IP) deliberately have no replacement:
// claude-code-redact substitutes a deterministic __RDX_*__ token instead.
func (l Legend) Replacement(seed []byte, category, value string) string {
	switch strings.ToUpper(category) {
	case "NAME":
		return l.FullName
	case "EMAIL":
		return l.Email
	case "HOST":
		if strings.HasSuffix(strings.ToLower(value), ".local") {
			return l.Hostname + ".local"
		}
		return l.Hostname
	case "PATH":
		cleaned := strings.TrimSuffix(value, "/")
		if isHostHome(value) || isHostHome(cleaned) {
			return l.Home()
		}
		base := cleaned
		if i := strings.LastIndex(cleaned, "/"); i >= 0 {
			base = cleaned[i+1:]
		}
		return l.DevPath(neutralSlug(seed, "project", base))
	case "PROJECT":
		return neutralSlug(seed, "project", value)
	default:
		return ""
	}
}

// isHostHome reports whether a value looks like a host user's home directory.
func isHostHome(value string) bool {
	if strings.HasPrefix(value, "/Users/") && strings.Count(value, "/") == 2 {
		return true
	}
	if strings.HasPrefix(value, "/home/") && strings.Count(value, "/") == 2 {
		return true
	}
	return false
}

// neutralSlug derives a stable neutral name from the seed and a value so the
// same harvested identity always maps to the same replacement.
func neutralSlug(seed []byte, label, value string) string {
	key := append(append([]byte(label+":"), seed...), value...)
	sum := sha256.Sum256(key)
	return words[(binary.BigEndian.Uint32(sum[:4]))%uint32(len(words))]
}

// stream is a deterministic byte stream: HMAC-SHA256(seed, label) expanded
// with a counter. It never uses math/rand so output is stable across Go
// toolchains and platforms.
type stream struct {
	key     []byte
	counter uint64
	buf     []byte
}

func newStream(seed []byte, label string) *stream {
	m := hmac.New(sha256.New, seed)
	_, _ = m.Write([]byte(label))
	return &stream{key: m.Sum(nil)}
}

func (s *stream) next(n int) []byte {
	for len(s.buf) < n {
		m := hmac.New(sha256.New, s.key)
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], s.counter)
		s.counter++
		_, _ = m.Write(b[:])
		s.buf = append(s.buf, m.Sum(nil)...)
	}
	out := s.buf[:n:n]
	s.buf = s.buf[n:]
	return out
}

func (s *stream) intn(n int) int {
	if n <= 1 {
		return 0
	}
	return int(binary.BigEndian.Uint32(s.next(4)) % uint32(n))
}

func pick(s *stream, list []string) string { return list[s.intn(len(list))] }

func uuidv4(b []byte) string {
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

var (
	firstNames = []string{
		"alina", "boris", "carla", "dario", "elin", "felix", "grete", "hugo",
		"iris", "jonas", "karin", "leon", "mira", "nora", "olaf", "pavel",
		"rita", "sven", "tessa", "urs", "vera", "wilma", "yara", "zoe",
	}
	lastNames = []string{
		"anders", "brandt", "costa", "dahl", "engel", "fischer", "gross",
		"hoffmann", "ivanov", "jansen", "keller", "lindqvist", "marchand",
		"novak", "otto", "petrov", "quast", "richter", "sommer", "toth",
		"ulrich", "vogel", "winter", "zimmer",
	}
	mailDomains    = []string{"gmail.com", "posteo.net", "proton.me", "fastmail.com"}
	hostAdjectives = []string{
		"amber", "brisk", "calm", "dusty", "early", "fuzzy", "golden",
		"hidden", "ivory", "jolly", "kind", "lunar", "mellow", "nimble",
		"olive", "plain", "quiet", "rapid", "still", "tidy", "umber",
		"vivid", "warm", "young",
	}
	hostNouns = []string{
		"badger", "cedar", "dune", "ember", "falcon", "grove", "harbor",
		"island", "juniper", "kestrel", "lagoon", "meadow", "nutmeg",
		"orchid", "pebble", "quarry", "reef", "spruce", "thicket", "vale",
		"willow", "yarrow", "zephyr", "birch",
	}
	domainWords = []string{
		"alder", "basalt", "cobalt", "dovetail", "ember", "flint", "granite",
		"hollowpine", "ironwood", "juniper", "kiln", "lumen", "maple",
		"northwind", "oakline", "pallas", "quartz", "riverstone", "slate",
		"tinder", "upland", "vela", "westbrook", "yonder",
	}
	domainTLDs = []string{"dev", "io", "net", "studio", "works"}
	timeZones  = []string{
		"Europe/Berlin", "Europe/Lisbon", "Europe/Prague", "Europe/Warsaw",
		"Europe/Tallinn", "Europe/Vienna", "America/Toronto", "Asia/Tbilisi",
	}
	// words are neutral project names for replacements of PROJECT and PATH
	// categories that are not derived from the legend itself.
	words = []string{
		"atlas", "beacon", "cinder", "delta", "echo", "fjord", "garnet",
		"harbor", "indigo", "juniper", "kelp", "lantern", "mosaic", "nimbus",
		"onyx", "prairie", "quill", "raven", "sable", "tundra", "umber",
		"vertex", "willow", "yarrow",
	}
)
