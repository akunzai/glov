package exec

import "strings"

// mergeEnv returns base overlaid with over. With fold, names compare
// case-insensitively (Windows); the overriding spelling wins.
func mergeEnv(base []string, over map[string]string, fold bool) []string {
	name := func(kv string) string {
		skip := 0
		if strings.HasPrefix(kv, "=") { // Windows keeps "=C:=C:\dir" entries
			skip = 1
		}
		if i := strings.IndexByte(kv[skip:], '='); i >= 0 {
			kv = kv[:skip+i]
		}
		if fold {
			kv = strings.ToUpper(kv)
		}
		return kv
	}
	out := make([]string, 0, len(base)+len(over))
	idx := map[string]int{}
	for _, kv := range base {
		idx[name(kv)] = len(out)
		out = append(out, kv)
	}
	for k, v := range over {
		kv := k + "=" + v
		if i, ok := idx[name(kv)]; ok {
			out[i] = kv
		} else {
			out = append(out, kv)
		}
	}
	return out
}
