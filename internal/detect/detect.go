// Package detect infers a starting configuration from a project directory and imports
// OpenAPI documents. Every inference carries its evidence and a confidence level, and
// nothing it reads is a secret: environment files contribute variable names only,
// never their values, so a generated configuration refers to ${DATABASE_URL} rather
// than containing it.
package detect

import (
	"bufio"
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/IshaanNene/Tracepoint/internal/errs"
)

// Confidence levels.
const (
	High   = "high"
	Medium = "medium"
	Low    = "low"
)

// Inference is one thing detection concluded, and why.
type Inference struct {
	What       string `json:"what"`
	Evidence   string `json:"evidence"`
	Confidence string `json:"confidence"`
}

// Detection is what a directory suggests.
type Detection struct {
	BaseURL      string
	DBDriver     string
	DBDSNEnv     string
	RedisAddrEnv string
	// OpenAPI is the path of the OpenAPI document found, relative to the directory.
	OpenAPI    string
	Import     *Import
	Inferences []Inference
	Notes      []string
}

var (
	composeNames = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}
	envFiles     = []string{".env", ".env.example", ".env.sample", ".env.local", ".env.template"}
	openapiNames = regexp.MustCompile(`(?i)^(openapi|swagger)(\.v?\d+)?\.(ya?ml|json)$`)
	dsnEnv       = regexp.MustCompile(`(?i)^(DATABASE_URL|DB_URL|DB_DSN|.*_DATABASE_URL|PG_DSN|POSTGRES_URL|POSTGRES_DSN|PG_URL|MYSQL_URL|MYSQL_DSN|.*_DSN)$`)
	redisEnv     = regexp.MustCompile(`(?i)^(REDIS_ADDR|REDIS_HOST_PORT|REDIS_URL|CACHE_URL|.*_REDIS_ADDR|.*_REDIS_URL)$`)
	skipDirs     = map[string]bool{".git": true, "node_modules": true, "vendor": true, "dist": true, "build": true, "target": true, "runs": true}
)

// maxDepth bounds how deep detection looks for an OpenAPI document.
const maxDepth = 3

// Detect reads a project directory: compose files for the services and their
// published ports, environment files for variable names, and OpenAPI documents for
// the requests worth loading.
func Detect(dir string) (*Detection, error) {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return nil, errs.New(errs.CodeConfigNotFound, "%s is not a directory", dir)
	}
	d := &Detection{}
	envNames := map[string]string{} // name -> where it was seen

	for _, name := range composeNames {
		b, rerr := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // the directory the user named
		if rerr != nil {
			continue
		}
		d.readCompose(name, b, envNames)
		break
	}
	for _, name := range envFiles {
		b, rerr := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // the directory the user named
		if rerr != nil {
			continue
		}
		for _, n := range envNamesIn(b) {
			if _, seen := envNames[n]; !seen {
				envNames[n] = name
			}
		}
	}
	d.pickEnv(envNames)

	if spec := findOpenAPI(dir); spec != "" {
		rel, rerr := filepath.Rel(dir, spec)
		if rerr != nil {
			rel = spec
		}
		b, rerr := os.ReadFile(spec) //nolint:gosec // found under the directory the user named
		if rerr == nil {
			imp, ierr := ImportOpenAPI(b, OpenAPIOptions{})
			if ierr == nil {
				d.OpenAPI, d.Import = rel, imp
				d.Inferences = append(d.Inferences, Inference{
					What:     fmt.Sprintf("%d safe request(s) from the OpenAPI document", len(imp.Requests)),
					Evidence: rel, Confidence: High,
				})
				if d.BaseURL == "" && len(imp.Servers) > 0 {
					// The servers list often names production, and a load test must never
					// be pointed there by default.
					d.Notes = append(d.Notes, fmt.Sprintf("the OpenAPI document lists %s, which is not used: servers often name production; set ${BASE_URL} to a test environment", strings.Join(imp.Servers, ", ")))
				}
			} else {
				d.Notes = append(d.Notes, fmt.Sprintf("%s looks like an OpenAPI document but could not be read: %v", rel, ierr))
			}
		}
	}
	if d.BaseURL == "" {
		d.BaseURL = "${BASE_URL}"
		d.Notes = append(d.Notes, "no application port was found; the configuration reads the base URL from ${BASE_URL}")
	}
	return d, nil
}

// readCompose finds the database, the cache and the application among compose
// services.
func (d *Detection) readCompose(file string, data []byte, envNames map[string]string) {
	var doc struct {
		Services map[string]struct {
			Image       string `yaml:"image"`
			Build       any    `yaml:"build"`
			Ports       []any  `yaml:"ports"`
			Environment any    `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		d.Notes = append(d.Notes, fmt.Sprintf("%s could not be read: %v", file, err))
		return
	}
	names := make([]string, 0, len(doc.Services))
	for n := range doc.Services {
		names = append(names, n)
	}
	sort.Strings(names)

	var apps []string
	appPort := map[string]int{}
	for _, name := range names {
		svc := doc.Services[name]
		image := strings.ToLower(svc.Image)
		evidence := fmt.Sprintf("%s: service %s uses image %s", file, name, svc.Image)
		switch {
		case strings.Contains(image, "postgres") || strings.Contains(image, "postgis") || strings.Contains(image, "timescale"):
			d.DBDriver = "postgres"
			d.Inferences = append(d.Inferences, Inference{What: "a Postgres database", Evidence: evidence, Confidence: High})
			continue
		case strings.Contains(image, "mysql") || strings.Contains(image, "mariadb"):
			d.DBDriver = "mysql"
			d.Inferences = append(d.Inferences, Inference{What: "a MySQL database", Evidence: evidence, Confidence: High})
			continue
		case strings.Contains(image, "redis") || strings.Contains(image, "valkey") || strings.Contains(image, "keydb"):
			d.RedisAddrEnv = "REDIS_ADDR"
			d.Inferences = append(d.Inferences, Inference{What: "a Redis cache", Evidence: evidence, Confidence: High})
			continue
		}
		for _, n := range envKeys(svc.Environment) {
			if _, seen := envNames[n]; !seen {
				envNames[n] = file + " service " + name
			}
		}
		if port := publishedPort(svc.Ports); port > 0 {
			apps = append(apps, name)
			appPort[name] = port
		} else if svc.Build != nil {
			apps = append(apps, name)
		}
	}
	for _, name := range apps {
		port := appPort[name]
		if port == 0 {
			continue
		}
		d.BaseURL = "http://127.0.0.1:" + strconv.Itoa(port)
		confidence := High
		if len(apps) > 1 {
			confidence = Medium
			d.Notes = append(d.Notes, fmt.Sprintf("%d services could be the application (%s); %s was picked", len(apps), strings.Join(apps, ", "), name))
		}
		d.Inferences = append(d.Inferences, Inference{
			What:     "the application at " + d.BaseURL,
			Evidence: fmt.Sprintf("%s: service %s publishes port %d", file, name, port), Confidence: confidence,
		})
		break
	}
}

// pickEnv chooses the environment variables the configuration should reference.
func (d *Detection) pickEnv(envNames map[string]string) {
	names := make([]string, 0, len(envNames))
	for n := range envNames {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		switch {
		case d.DBDSNEnv == "" && dsnEnv.MatchString(n):
			d.DBDSNEnv = n
			d.Inferences = append(d.Inferences, Inference{
				What: "the database DSN in ${" + n + "}", Evidence: "variable " + n + " in " + envNames[n], Confidence: Medium,
			})
			if d.DBDriver == "" {
				d.Notes = append(d.Notes, "a DSN variable was found but no database service; set db.driver")
			}
		case redisEnv.MatchString(n) && (d.RedisAddrEnv == "" || d.RedisAddrEnv == "REDIS_ADDR"):
			d.RedisAddrEnv = n
			d.Inferences = append(d.Inferences, Inference{
				What: "the Redis address in ${" + n + "}", Evidence: "variable " + n + " in " + envNames[n], Confidence: Medium,
			})
			if strings.HasSuffix(strings.ToUpper(n), "_URL") {
				d.Notes = append(d.Notes, "${"+n+"} probably holds a redis:// URL; redis.addr needs host:port")
			}
		}
	}
	if d.DBDriver != "" && d.DBDSNEnv == "" {
		d.DBDSNEnv = "DATABASE_URL"
		d.Notes = append(d.Notes, "a database service was found but no DSN variable; the configuration reads ${DATABASE_URL}")
	}
}

// publishedPort reads the host side of the first published port.
func publishedPort(ports []any) int {
	for _, p := range ports {
		switch v := p.(type) {
		case string:
			parts := strings.Split(strings.Split(v, "/")[0], ":")
			host := parts[0]
			if len(parts) >= 2 {
				host = parts[len(parts)-2]
			}
			if n, err := strconv.Atoi(host); err == nil {
				return n
			}
		case int:
			return v
		case map[string]any:
			if n, ok := number(v["published"]); ok {
				return int(n)
			}
			if n, err := strconv.Atoi(str(v["published"])); err == nil {
				return n
			}
		}
	}
	return 0
}

// envKeys lists a compose environment's variable names, from either form.
func envKeys(env any) []string {
	var out []string
	switch v := env.(type) {
	case map[string]any:
		for k := range v {
			out = append(out, k)
		}
	case []any:
		for _, item := range v {
			name, _, _ := strings.Cut(str(item), "=")
			out = append(out, strings.TrimSpace(name))
		}
	}
	return out
}

// envNamesIn reads the variable names of an env file, and never keeps a value.
func envNamesIn(data []byte) []string {
	var out []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		if name, _, ok := strings.Cut(line, "="); ok {
			out = append(out, strings.TrimSpace(name))
		}
	}
	return out
}

// findOpenAPI returns the shallowest OpenAPI document under dir.
func findOpenAPI(dir string) string {
	found, depth := "", maxDepth+1
	if err := filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // an unreadable entry is skipped, not fatal
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return nil //nolint:nilerr // a path that cannot be related to dir is skipped
		}
		level := len(strings.Split(rel, string(filepath.Separator)))
		if e.IsDir() {
			if path != dir && (skipDirs[e.Name()] || level > maxDepth) {
				return filepath.SkipDir
			}
			return nil
		}
		if openapiNames.MatchString(e.Name()) && level < depth {
			found, depth = path, level
		}
		return nil
	}); err != nil {
		return "" // unreachable: the callback never returns an error
	}
	return found
}
