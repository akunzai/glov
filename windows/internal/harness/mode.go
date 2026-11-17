package harness

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"maps"
	"slices"
	"strings"
)

// How a harness VM's guest is set up for each harness: with the API key, or
// signed in to the owner's subscription (Profile.SignedIn). The host
// decides; these render what the guest must hold for that choice, as
// macOS's HarnessMode does.

// MarkerPath lists the harnesses the guest is set up as signed in, one name
// per line. The host compares it with vm.json at each Guest Service session,
// so a guest reset to its template in the other mode is set up again.
const MarkerPath = "/etc/glov/harness-subscriptions"

// Marker is the marker file for subscribed.
func Marker(subscribed []string) string {
	var b strings.Builder
	for _, n := range slices.Sorted(slices.Values(subscribed)) {
		b.WriteString(n + "\n")
	}
	return b.String()
}

// ParseMarker is the harnesses a marker file names, sorted, each once.
func ParseMarker(text string) []string {
	var out []string
	for line := range strings.Lines(text) {
		if n := strings.TrimSpace(line); n != "" && !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out
}

// Environment is /etc/environment (text) with the variables of profiles as
// each one's mode sets them: every variable either mode sets is dropped,
// then the chosen mode's are appended. Other lines stay as they are.
func Environment(text string, profiles []Profile, subscribed []string, vm string) string {
	managed := map[string]bool{}
	wanted := map[string]string{}
	for _, p := range profiles {
		signedIn := p.SignedIn(vm)
		for k := range p.GuestEnvironment() {
			managed[k] = true
		}
		for k := range signedIn.GuestEnvironment() {
			managed[k] = true
		}
		chosen := p
		if slices.Contains(subscribed, p.Name) {
			chosen = signedIn
		}
		maps.Copy(wanted, chosen.GuestEnvironment())
	}
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	lines = slices.DeleteFunc(lines, func(line string) bool {
		k, _, ok := strings.Cut(line, "=")
		return ok && managed[strings.TrimSpace(k)]
	})
	for _, k := range slices.Sorted(maps.Keys(wanted)) {
		lines = append(lines, k+"="+wanted[k])
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	return b.String()
}

// Edit is one setting in a home file as the chosen mode wants it; an empty
// Value is absent.
type Edit struct {
	Path  string
	Key   []string
	Value jsontext.Value
}

// Edits is the settings of profiles that differ between the two modes, as
// each one's mode wants them. Only these change in the guest's files, so
// whatever else the owner set there stays.
func Edits(profiles []Profile, subscribed []string, vm string) []Edit {
	var out []Edit
	for _, p := range profiles {
		sub := p.Subscription
		if sub == nil {
			continue
		}
		on := slices.Contains(subscribed, p.Name)
		for _, s := range sub.Settings {
			v := s.Value
			if !on {
				v = apiValue(p, s)
			}
			out = append(out, Edit{Path: s.Path, Key: s.Key, Value: v})
		}
		// Placeholders that go into JSON files: the chosen mode's in, the
		// other's out.
		inFiles := func(secrets []Secret) []Secret {
			return slices.DeleteFunc(slices.Clone(secrets), func(s Secret) bool {
				return s.Delivery != Placeholder || s.File == "" || len(s.JSON) == 0
			})
		}
		api, signedIn := inFiles(p.Secrets), inFiles(p.SignedIn(vm).Secrets)
		out1, in := signedIn, api
		if on {
			out1, in = api, signedIn
		}
		for _, s := range out1 {
			out = append(out, Edit{Path: s.File, Key: s.JSON})
		}
		for _, s := range in {
			v, _ := json.Marshal(s.PlaceholderText())
			out = append(out, Edit{Path: s.File, Key: s.JSON, Value: v})
		}
	}
	return out
}

// apiValue is what the profile's own file says for s, empty when nothing.
func apiValue(p Profile, s Setting) jsontext.Value {
	i := slices.IndexFunc(p.GuestFiles(), func(f File) bool { return f.Path == s.Path })
	if i < 0 {
		return nil
	}
	f := p.GuestFiles()[i]
	if s.IsTOML() {
		if f.Text == nil {
			return nil
		}
		v, ok := TOMLValue(*f.Text, s.Key[0])
		if !ok {
			return nil
		}
		data, _ := json.Marshal(v)
		return data
	}
	var doc any
	if json.Unmarshal(f.JSON, &doc) != nil {
		return nil
	}
	for _, k := range s.Key {
		o, ok := doc.(map[string]any)
		if !ok {
			return nil
		}
		if doc, ok = o[k]; !ok {
			return nil
		}
	}
	data, _ := json.Marshal(doc, json.Deterministic(true))
	return data
}

// Apply is text (a file of path) with edits for that path applied; false
// for a JSON file that does not parse, so a hand-broken file is not
// replaced.
func Apply(edits []Edit, text, path string) (string, bool) {
	mine := slices.DeleteFunc(slices.Clone(edits), func(e Edit) bool { return e.Path != path })
	if len(mine) == 0 {
		return text, true
	}
	if strings.HasSuffix(path, ".toml") {
		for _, e := range mine {
			var v *string
			var s string
			if len(e.Value) > 0 && json.Unmarshal(e.Value, &s) == nil {
				v = &s
			}
			text = SettingTOML(text, e.Key[0], v)
		}
		return text, true
	}
	src := strings.TrimSpace(text)
	if src == "" {
		src = "{}"
	}
	var doc any
	if json.Unmarshal([]byte(src), &doc) != nil {
		return "", false
	}
	for _, e := range mine {
		doc = setJSON(doc, e.Key, e.Value)
	}
	data, err := json.Marshal(doc, json.Deterministic(true))
	if err != nil {
		return "", false
	}
	return File{Path: path, JSON: data}.Content(), true
}

// setJSON is v with the key path keys set to value, or removed when value
// is empty; an object is made where the path needs one.
func setJSON(v any, keys []string, value jsontext.Value) any {
	o, _ := v.(map[string]any)
	if o == nil {
		o = map[string]any{}
	}
	if len(keys) == 1 {
		if len(value) == 0 {
			delete(o, keys[0])
		} else {
			var x any
			json.Unmarshal(value, &x)
			o[keys[0]] = x
		}
		return o
	}
	o[keys[0]] = setJSON(o[keys[0]], keys[1:], value)
	return o
}

// TOMLValue is the string value of top-level key in TOML text, if it has
// one.
func TOMLValue(text, key string) (string, bool) {
	for _, line := range topLevel(text) {
		k, v, ok := keyValue(line)
		if ok && k == key && len(v) >= 2 && strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) {
			return v[1 : len(v)-1], true
		}
	}
	return "", false
}

// SettingTOML is text with top-level key set to the string value, or
// removed when nil; a new key goes before the first table.
func SettingTOML(text, key string, value *string) string {
	lines := strings.Split(text, "\n")
	firstTable := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "[") })
	if firstTable < 0 {
		firstTable = len(lines)
	}
	var line string
	if value != nil {
		line = key + ` = "` + strings.ReplaceAll(strings.ReplaceAll(*value, `\`, `\\`), `"`, `\"`) + `"`
	}
	i := slices.IndexFunc(lines[:firstTable], func(l string) bool { k, _, ok := keyValue(l); return ok && k == key })
	switch {
	case i >= 0 && value != nil:
		lines[i] = line
	case i >= 0:
		lines = slices.Delete(lines, i, i+1)
	case value != nil:
		at := firstTable
		for at > 0 && strings.TrimSpace(lines[at-1]) == "" {
			at--
		}
		lines = slices.Insert(lines, at, line)
	}
	return strings.Join(lines, "\n")
}

func topLevel(text string) []string {
	lines := strings.Split(text, "\n")
	if i := slices.IndexFunc(lines, func(l string) bool { return strings.HasPrefix(strings.TrimSpace(l), "[") }); i >= 0 {
		lines = lines[:i]
	}
	return lines
}

func keyValue(line string) (key, value string, ok bool) {
	t := strings.TrimSpace(line)
	if strings.HasPrefix(t, "#") {
		return "", "", false
	}
	k, v, ok := strings.Cut(t, "=")
	return strings.TrimSpace(k), strings.TrimSpace(v), ok
}
