package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/spf13/viper"
)

// Domain roles: the online domain is the writable upstream cache, offline
// domains are read-only published trees that fall back to the online tree.
const (
	RoleOnline  = "online"
	RoleOffline = "offline"
)

// HTTPConfig configures the mirror server listener.
type HTTPConfig struct {
	BindAddr          string        `mapstructure:"bind_addr" yaml:"bind_addr"`
	Port              uint          `mapstructure:"port" yaml:"port" validate:"gt=0,lte=65535"`
	ReadHeaderTimeout time.Duration `mapstructure:"read_header_timeout" yaml:"read_header_timeout" validate:"gt=0"`
	ReadTimeout       time.Duration `mapstructure:"read_timeout" yaml:"read_timeout" validate:"gt=0"`
	WriteTimeout      time.Duration `mapstructure:"write_timeout" yaml:"write_timeout" validate:"gt=0"`
	IdleTimeout       time.Duration `mapstructure:"idle_timeout" yaml:"idle_timeout" validate:"gt=0"`
	// DirectoryIndexes enables directory listings: upstream index pages are
	// passed through, and the root serves a generated index of the mounts
	// when no catch-all mount covers it. When disabled, every directory
	// request answers with a static notice page instead.
	DirectoryIndexes bool `mapstructure:"directory_indexes" yaml:"directory_indexes"`
}

// ListenAddr renders the bind address and port for net.Listen, bracketing
// IPv6 addresses as required.
func (h HTTPConfig) ListenAddr() string {
	return net.JoinHostPort(h.BindAddr, strconv.Itoa(int(h.Port)))
}

// LogConfig configures the server's logging output.
type LogConfig struct {
	Level   string   `mapstructure:"level" yaml:"level" validate:"oneof=debug info warn error"`
	Type    string   `mapstructure:"type" yaml:"type" validate:"oneof=console json"`
	Outputs []string `mapstructure:"outputs" yaml:"outputs" validate:"required,min=1,dive,required"`
}

// DomainConfig maps a request Host header to a filesystem root and role.
type DomainConfig struct {
	Domain string `mapstructure:"domain" yaml:"domain" validate:"required"`
	Root   string `mapstructure:"root" yaml:"root" validate:"required"`
	Role   string `mapstructure:"role" yaml:"role" validate:"required,oneof=online offline"`
}

// MountRepoConfig is a repository kept synchronized regardless of client
// traffic, identified by its request path and format.
type MountRepoConfig struct {
	Path string `mapstructure:"path" yaml:"path" validate:"required"`
	Type string `mapstructure:"type" yaml:"type" validate:"required,oneof=rpm deb arch apk"`
}

// MountConfig maps a base request path onto an upstream mirror URL.
type MountConfig struct {
	Path     string            `mapstructure:"path" yaml:"path" validate:"required"`
	Upstream string            `mapstructure:"upstream" yaml:"upstream" validate:"required,url"`
	Repos    []MountRepoConfig `mapstructure:"repos" yaml:"repos" validate:"omitempty,dive"`
}

// CrawlerConfig tunes the background synchronization behavior.
type CrawlerConfig struct {
	Workers          int             `mapstructure:"workers" yaml:"workers" validate:"gt=0"`
	ConcurrentCrawls int             `mapstructure:"concurrent_crawls" yaml:"concurrent_crawls" validate:"gt=0"`
	RequestTimeout   time.Duration   `mapstructure:"request_timeout" yaml:"request_timeout" validate:"gt=0"`
	UserAgent        string          `mapstructure:"user_agent" yaml:"user_agent"`
	PruneGrace       time.Duration   `mapstructure:"prune_grace" yaml:"prune_grace" validate:"gte=0"`
	RefreshSchedule  []time.Duration `mapstructure:"refresh_schedule" yaml:"refresh_schedule" validate:"required,min=1,dive,gt=0"`
	// MissingMode decides what a synchronization does with a package the
	// repository metadata lists but the upstream does not serve. The mode
	// names are the mirror's own; the fetch package parses them.
	MissingMode string `mapstructure:"missing_mode" yaml:"missing_mode" validate:"omitempty,oneof=fail retry ignore"`
	// MissingRetries is how many consecutive runs must miss a file before
	// the retry mode accepts it as gone.
	MissingRetries int `mapstructure:"missing_retries" yaml:"missing_retries" validate:"gte=0"`
	// DiscoverCache is how long a discovery crawl's results are reused
	// before the tree is scanned again.
	DiscoverCache time.Duration `mapstructure:"discover_cache" yaml:"discover_cache" validate:"gte=0"`
}

// TraceConfig describes the mirror this instance publishes, and is written
// into each synchronized or crawled repository as a trace file when
// enabled. The fields follow the trace convention Debian archives
// established, which downstream mirrors and mirror checkers read to learn
// who runs a mirror, where it is, and when it last synchronized.
type TraceConfig struct {
	Enabled bool `mapstructure:"enabled" yaml:"enabled"`
	// Host names this mirror in the trace, and is also the trace file's
	// name. It defaults to the system hostname.
	Host       string `mapstructure:"host" yaml:"host"`
	Maintainer string `mapstructure:"maintainer" yaml:"maintainer"`
	Sponsor    string `mapstructure:"sponsor" yaml:"sponsor"`
	Country    string `mapstructure:"country" yaml:"country"`
	Location   string `mapstructure:"location" yaml:"location"`
	Throughput string `mapstructure:"throughput" yaml:"throughput"`
}

// HostName returns the name this mirror is traced under, falling back to
// the system hostname when none is configured. The fallback lives here
// rather than in the defaults so it also applies to the built-in
// configuration, which is what a run without a config file reads.
func (t TraceConfig) HostName() string {
	if t.Host != "" {
		return t.Host
	}
	if name, err := os.Hostname(); err == nil && name != "" {
		return name
	}
	return "unknown"
}

// Config is the mirror server configuration.
type Config struct {
	HTTP                 HTTPConfig     `mapstructure:"http" yaml:"http"`
	Log                  LogConfig      `mapstructure:"log" yaml:"log"`
	StatePath            string         `mapstructure:"state_path" yaml:"state_path" validate:"required"`
	StateFlushInterval   time.Duration  `mapstructure:"state_flush_interval" yaml:"state_flush_interval" validate:"gt=0"`
	RepoCrawlInterval    time.Duration  `mapstructure:"repo_crawl_interval" yaml:"repo_crawl_interval" validate:"gt=0"`
	FailureRetryInterval time.Duration  `mapstructure:"failure_retry_interval" yaml:"failure_retry_interval" validate:"gt=0"`
	CrawlCheckInterval   time.Duration  `mapstructure:"crawl_check_interval" yaml:"crawl_check_interval" validate:"gt=0"`
	GenericMaxAge        time.Duration  `mapstructure:"generic_max_age" yaml:"generic_max_age" validate:"gt=0"`
	NegativeCacheTTL     time.Duration  `mapstructure:"negative_cache_ttl" yaml:"negative_cache_ttl" validate:"gt=0"`
	TransientErrorTTL    time.Duration  `mapstructure:"transient_error_ttl" yaml:"transient_error_ttl" validate:"gt=0"`
	Domains              []DomainConfig `mapstructure:"domains" yaml:"domains" validate:"omitempty,dive"`
	Mounts               []MountConfig  `mapstructure:"mounts" yaml:"mounts" validate:"omitempty,dive"`
	Crawler              CrawlerConfig  `mapstructure:"crawler" yaml:"crawler"`
	Trace                TraceConfig    `mapstructure:"trace" yaml:"trace"`

	domainsByName map[string]int
	onlineIndex   int
	mountOrder    []int
}

// C is the active server configuration. A reload builds and validates a
// replacement in full before repointing C at it, so readers always see a
// complete configuration; requests already in flight finish against the
// one they picked up.
var C *Config

// C starts out holding the built-in defaults so it is never nil. Commands
// that run without a configuration file, and packages exercised by tests
// that never call Init, read those defaults instead of guarding for nil.
func init() {
	C = defaults()
}

// defaults builds a configuration from the registered defaults alone,
// consulting no file. Nothing here can fail: the values are static and the
// indexes start empty, so validation is left to Init.
func defaults() *Config {
	v := viper.New()
	setDefaults(v)
	c := &Config{}
	// Unmarshaling static defaults into their own struct cannot fail.
	_ = v.Unmarshal(c)
	c.StatePath = "/etc/repo-sync/state.yaml"
	c.domainsByName = map[string]int{}
	c.onlineIndex = -1
	return c
}

// loadConfig reads and validates a configuration file. Init installs the
// result; callers work with the C singleton rather than loading their own.
func loadConfig(configPath string) (*Config, error) {
	v := viper.New()
	setDefaults(v)

	file := findConfig(configPath)
	if file != "" {
		v.SetConfigFile(file)
		if err := v.ReadInConfig(); err != nil {
			return nil, fmt.Errorf("read config %s: %w", file, err)
		}
	}

	c := &Config{}
	if err := v.Unmarshal(c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Default the state file next to the config file, so ./config.yaml
	// keeps state in ./state.yaml, or to the system path without one.
	if c.StatePath == "" {
		if file != "" {
			c.StatePath = filepath.Join(filepath.Dir(file), "state.yaml")
		} else {
			c.StatePath = "/etc/repo-sync/state.yaml"
		}
	}

	if err := c.finalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// Init loads and validates the configuration, installs it as the active
// configuration, and applies its logging settings. A failed reload leaves
// the previous configuration in place.
func Init(configPath string) error {
	c, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	install(c)
	return nil
}

// InitServer is Init with the mirror server's additional requirements
// checked before the configuration is installed, so a bad reload leaves
// the running server on the configuration it is already serving.
func InitServer(configPath string) error {
	c, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	if err := c.RequireServer(); err != nil {
		return err
	}
	install(c)
	return nil
}

// install publishes a configuration and applies the settings derived from
// it.
func install(c *Config) {
	C = c
	SetupLogging(c.Log)
}

// findConfig resolves the configuration file path: the explicit flag, the
// working directory, the user config directory, then the system directory.
func findConfig(explicit string) string {
	if explicit != "" {
		return explicit
	}
	candidates := []string{"config.yaml"}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".config", "repo-sync", "config.yaml"))
	}
	candidates = append(candidates, "/etc/repo-sync/config.yaml")
	for _, name := range candidates {
		if info, err := os.Stat(name); err == nil && info.Mode().IsRegular() {
			return name
		}
	}
	return ""
}

// setDefaults registers every configuration default.
func setDefaults(v *viper.Viper) {
	v.SetDefault("state_flush_interval", 30*time.Second)
	v.SetDefault("repo_crawl_interval", 4*time.Hour)
	v.SetDefault("failure_retry_interval", time.Hour)
	v.SetDefault("crawl_check_interval", 30*time.Minute)
	v.SetDefault("generic_max_age", 6*time.Hour)
	v.SetDefault("negative_cache_ttl", time.Hour)
	v.SetDefault("transient_error_ttl", time.Minute)

	v.SetDefault("http.port", 8080)
	v.SetDefault("http.directory_indexes", true)
	v.SetDefault("http.read_header_timeout", 10*time.Second)
	v.SetDefault("http.read_timeout", 5*time.Minute)
	v.SetDefault("http.write_timeout", 30*time.Minute)
	v.SetDefault("http.idle_timeout", 2*time.Minute)

	v.SetDefault("log.level", "info")
	v.SetDefault("log.type", "console")
	v.SetDefault("log.outputs", []string{"console"})

	v.SetDefault("crawler.workers", 4)
	v.SetDefault("crawler.concurrent_crawls", 4)
	v.SetDefault("crawler.request_timeout", 2*time.Minute)
	v.SetDefault("crawler.user_agent", Name+"/"+Version)
	v.SetDefault("crawler.prune_grace", 0)
	v.SetDefault("crawler.missing_mode", "retry")
	v.SetDefault("crawler.missing_retries", 3)
	v.SetDefault("crawler.discover_cache", 72*time.Hour)
	v.SetDefault("crawler.refresh_schedule", []time.Duration{
		12 * time.Hour, 24 * time.Hour, 48 * time.Hour,
		72 * time.Hour, 96 * time.Hour, 120 * time.Hour,
	})

	v.SetDefault("trace.enabled", false)
}

// finalize canonicalizes, validates, and indexes a parsed configuration.
func (c *Config) finalize() error {
	c.canonicalize()
	if err := validateConfig(c); err != nil {
		return err
	}
	if err := c.indexDomains(); err != nil {
		return err
	}
	return c.indexMounts()
}

// canonicalize normalizes user-supplied values before validation.
func (c *Config) canonicalize() {
	c.StatePath = expandHome(strings.TrimSpace(c.StatePath))
	for i := range c.Domains {
		c.Domains[i].Domain = strings.ToLower(strings.TrimSpace(c.Domains[i].Domain))
		c.Domains[i].Role = strings.ToLower(strings.TrimSpace(c.Domains[i].Role))
		c.Domains[i].Root = expandHome(strings.TrimSpace(c.Domains[i].Root))
	}
	// Trace values are written verbatim into a published file, so trim them
	// and drop anything that would break the one-field-per-line format.
	c.Trace.Host = TraceValue(c.Trace.Host)
	c.Trace.Maintainer = TraceValue(c.Trace.Maintainer)
	c.Trace.Sponsor = TraceValue(c.Trace.Sponsor)
	c.Trace.Country = TraceValue(c.Trace.Country)
	c.Trace.Location = TraceValue(c.Trace.Location)
	c.Trace.Throughput = TraceValue(c.Trace.Throughput)
	for i := range c.Mounts {
		c.Mounts[i].Path = cleanRequestPath(c.Mounts[i].Path)
		c.Mounts[i].Upstream = strings.TrimRight(strings.TrimSpace(c.Mounts[i].Upstream), "/")
		for j := range c.Mounts[i].Repos {
			c.Mounts[i].Repos[j].Path = cleanRequestPath(c.Mounts[i].Repos[j].Path)
			c.Mounts[i].Repos[j].Type = strings.ToLower(strings.TrimSpace(c.Mounts[i].Repos[j].Type))
		}
	}
}

// cleanRequestPath normalizes a request path to a single leading slash with
// no trailing slash.
func cleanRequestPath(p string) string {
	return path.Clean("/" + strings.Trim(strings.TrimSpace(p), "/"))
}

// TraceValue normalizes a trace field for publication. Each field occupies
// one line of the trace file, so embedded newlines are folded to spaces
// rather than trimmed away, which would let a configured value forge
// additional trace fields.
func TraceValue(s string) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		// Drop the remaining control characters outright.
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

// expandHome resolves a leading ~ against the user's home directory.
func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

// validateConfig applies the declarative validation rules, reporting
// failures by their YAML key names.
func validateConfig(c *Config) error {
	validate := validator.New()
	validate.RegisterTagNameFunc(func(field reflect.StructField) string {
		name := strings.SplitN(field.Tag.Get("mapstructure"), ",", 2)[0]
		if name == "-" {
			return ""
		}
		return name
	})
	err := validate.Struct(c)
	if err == nil {
		return nil
	}
	var verrs validator.ValidationErrors
	if !errors.As(err, &verrs) {
		return err
	}
	var fields []string
	for _, verr := range verrs {
		fields = append(fields, fmt.Sprintf("%s (%s)", strings.ToLower(verr.Namespace()), verr.Tag()))
	}
	return fmt.Errorf("invalid configuration: %s", strings.Join(fields, ", "))
}

// indexDomains builds the host lookup and enforces exactly one online
// domain.
func (c *Config) indexDomains() error {
	c.domainsByName = map[string]int{}
	c.onlineIndex = -1
	for i := range c.Domains {
		d := c.Domains[i]
		if _, ok := c.domainsByName[d.Domain]; ok {
			return fmt.Errorf("domain %s is configured more than once", d.Domain)
		}
		c.domainsByName[d.Domain] = i
		if d.Role == RoleOnline {
			if c.onlineIndex != -1 {
				return fmt.Errorf("only one online domain may be configured")
			}
			c.onlineIndex = i
		}
	}
	return nil
}

// RequireServer reports whether the configuration carries what the mirror
// server needs. Domains and mounts are only meaningful to the server, so
// they are checked here rather than at load; the sync commands run against
// a configuration that has neither.
func (c *Config) RequireServer() error {
	if len(c.Domains) == 0 {
		return errors.New("at least one domain must be configured")
	}
	if c.onlineIndex == -1 {
		return errors.New("one online domain must be configured")
	}
	if len(c.Mounts) == 0 {
		return errors.New("at least one mount must be configured")
	}
	return nil
}

// indexMounts validates mount uniqueness and upstream URLs, orders mounts
// longest path first for prefix lookup, and checks configured repositories
// sit under their mount.
func (c *Config) indexMounts() error {
	seen := map[string]bool{}
	for i := range c.Mounts {
		m := &c.Mounts[i]
		if seen[m.Path] {
			return fmt.Errorf("mount path %s is configured more than once", m.Path)
		}
		seen[m.Path] = true
		u, err := url.Parse(m.Upstream)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return fmt.Errorf("mount %s upstream must include scheme and host", m.Path)
		}
		// Request URLs are built by joining a relative path onto the
		// upstream; a query or fragment would swallow that path, so reject
		// them rather than compose a malformed URL later.
		if u.RawQuery != "" || u.Fragment != "" {
			return fmt.Errorf("mount %s upstream must not contain a query or fragment", m.Path)
		}
		for _, repo := range m.Repos {
			if !pathUnder(repo.Path, m.Path) {
				return fmt.Errorf("repository %s is not under mount %s", repo.Path, m.Path)
			}
		}
	}
	c.mountOrder = make([]int, len(c.Mounts))
	for i := range c.mountOrder {
		c.mountOrder[i] = i
	}
	sort.Slice(c.mountOrder, func(a, b int) bool {
		return len(c.Mounts[c.mountOrder[a]].Path) > len(c.Mounts[c.mountOrder[b]].Path)
	})
	return nil
}

// pathUnder reports whether a request path equals or sits below a base
// path.
func pathUnder(p, base string) bool {
	if base == "/" {
		return true
	}
	return p == base || strings.HasPrefix(p, base+"/")
}

// Domain looks up the configuration for a request Host header. The port is
// stripped and IPv6 literals are unbracketed before the lookup.
func (c *Config) Domain(host string) (DomainConfig, bool) {
	host = strings.ToLower(strings.TrimSpace(host))
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	i, ok := c.domainsByName[host]
	if !ok {
		return DomainConfig{}, false
	}
	return c.Domains[i], true
}

// OnlineDomain returns the single writable cache domain.
func (c *Config) OnlineDomain() DomainConfig {
	return c.Domains[c.onlineIndex]
}

// MountFor finds the longest mount matching a request path.
func (c *Config) MountFor(reqPath string) (MountConfig, bool) {
	for _, i := range c.mountOrder {
		if pathUnder(reqPath, c.Mounts[i].Path) {
			return c.Mounts[i], true
		}
	}
	return MountConfig{}, false
}
