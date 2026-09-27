package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sorotrail/sorotrail/internal/config"
)

// runConfig implements `sorotrail config`: prints the effective runtime
// configuration with all credentials and secrets redacted.
func runConfig(args []string) error {
	return runConfigTo(os.Stdout, args)
}

// configUsage is the help message for the config subcommand.
const configUsage = `usage: sorotrail config [flags]

Prints the effective configuration with secrets redacted.

flags:
  -format string
    	output format: text or json (default "text")
  -json
    	shorthand for -format json
`

// runConfigTo is runConfig with an explicit output writer so tests can
// assert on the output without touching stdout.
func runConfigTo(w io.Writer, args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), configUsage)
	}

	formatFlag := fs.String("format", "text", "output format: text or json")
	jsonFlag := fs.Bool("json", false, "shorthand for -format json")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil // usage already printed
		}
		return err
	}

	if len(fs.Args()) > 0 {
		fs.Usage()
		return fmt.Errorf("unexpected argument %q (config takes no positional arguments)", fs.Args()[0])
	}

	format := strings.ToLower(strings.TrimSpace(*formatFlag))
	if *jsonFlag {
		format = "json"
	}

	if format != "text" && format != "json" {
		return fmt.Errorf("unsupported format %q (want text or json)", *formatFlag)
	}

	cfg, err := loadEffectiveConfig()
	if err != nil {
		return err
	}

	return renderRedactedConfig(w, cfg, format)
}

// loadEffectiveConfig loads the runtime configuration. If standard loading
// succeeds, it returns the validated config. If config.Load() failed solely
// because DATABASE_URL is unset, it loads the default configuration and validates
// all other fields against the environment while leaving DATABASE_URL empty.
func loadEffectiveConfig() (config.Config, error) {
	cfg, err := config.Load()
	if err == nil {
		return cfg, nil
	}

	if strings.Contains(err.Error(), "DATABASE_URL") && strings.TrimSpace(os.Getenv("DATABASE_URL")) == "" {
		origVal, hadEnv := os.LookupEnv("DATABASE_URL")
		_ = os.Setenv("DATABASE_URL", "sqlite::memory:")
		testCfg, testErr := config.Load()
		if hadEnv {
			_ = os.Setenv("DATABASE_URL", origVal)
		} else {
			_ = os.Unsetenv("DATABASE_URL")
		}
		if testErr == nil {
			testCfg.DatabaseURL = ""
			return testCfg, nil
		}
	}

	return config.Config{}, err
}

// configItem represents a single configuration setting.
type configItem struct {
	Name  string
	Value string
	Raw   any
}

// redactDatabaseURL masks the password in a database URL, returning
// "<redacted>" if the sensitive URL is malformed.
func redactDatabaseURL(raw string) string {
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "sqlite:") {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "<redacted>"
	}
	if u.User != nil {
		if _, has := u.User.Password(); has {
			u.User = url.UserPassword(u.User.Username(), "***")
		}
	}
	return u.String()
}

// redactedConfigItems extracts all configuration fields from cfg based on their
// struct `env` tags and applies redactions to sensitive credentials.
func redactedConfigItems(cfg config.Config) []configItem {
	val := reflect.ValueOf(cfg)
	typ := val.Type()
	var items []configItem

	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		envTag := f.Tag.Get("env")
		if envTag == "" || envTag == "-" {
			continue
		}
		name := strings.TrimSpace(strings.Split(envTag, ",")[0])
		if name == "" {
			continue
		}

		fieldVal := val.Field(i)
		var strVal string
		var rawVal any

		switch name {
		case "DATABASE_URL":
			raw := fieldVal.String()
			strVal = redactDatabaseURL(raw)
			rawVal = strVal

		case "RPC_URL":
			raw := fieldVal.String()
			strVal = redactURLUserinfo(raw)
			rawVal = strVal

		case "RPC_URLS":
			var redactedList []string
			if fieldVal.Kind() == reflect.Slice {
				for j := 0; j < fieldVal.Len(); j++ {
					elem := fieldVal.Index(j).String()
					redactedList = append(redactedList, redactURLUserinfo(elem))
				}
			}
			strVal = strings.Join(redactedList, ",")
			rawVal = redactedList

		case "API_KEY", "ARCHIVE_SECRET_ACCESS_KEY", "MULTI_TENANT_BOOTSTRAP_KEY":
			raw := fieldVal.String()
			if raw != "" {
				strVal = "***"
			} else {
				strVal = ""
			}
			rawVal = strVal

		case "HORIZON_URL":
			raw := fieldVal.String()
			strVal = redactURLUserinfo(raw)
			rawVal = strVal

		default:
			switch fieldVal.Kind() {
			case reflect.String:
				strVal = fieldVal.String()
				rawVal = strVal
			case reflect.Bool:
				b := fieldVal.Bool()
				strVal = strconv.FormatBool(b)
				rawVal = b
			case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
				if fieldVal.Type() == reflect.TypeOf(time.Duration(0)) {
					d := time.Duration(fieldVal.Int())
					strVal = d.String()
					rawVal = strVal
				} else {
					n := fieldVal.Int()
					strVal = strconv.FormatInt(n, 10)
					rawVal = n
				}
			case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
				n := fieldVal.Uint()
				strVal = strconv.FormatUint(n, 10)
				rawVal = n
			case reflect.Float32, reflect.Float64:
				f := fieldVal.Float()
				strVal = strconv.FormatFloat(f, 'f', -1, 64)
				rawVal = f
			case reflect.Slice:
				var list []string
				for j := 0; j < fieldVal.Len(); j++ {
					list = append(list, fmt.Sprint(fieldVal.Index(j).Interface()))
				}
				strVal = strings.Join(list, ",")
				rawVal = list
			default:
				strVal = fmt.Sprint(fieldVal.Interface())
				rawVal = strVal
			}
		}

		items = append(items, configItem{
			Name:  name,
			Value: strVal,
			Raw:   rawVal,
		})
	}

	sort.Slice(items, func(i, j int) bool {
		return items[i].Name < items[j].Name
	})

	return items
}

// renderRedactedConfig writes the redacted configuration to w in either text or json format.
func renderRedactedConfig(w io.Writer, cfg config.Config, format string) error {
	items := redactedConfigItems(cfg)

	switch format {
	case "json":
		m := make(map[string]any, len(items))
		for _, it := range items {
			m[it.Name] = it.Raw
		}
		data, err := json.MarshalIndent(m, "", "  ")
		if err != nil {
			return fmt.Errorf("encoding config json: %w", err)
		}
		_, err = fmt.Fprintf(w, "%s\n", data)
		return err

	case "text":
		for _, it := range items {
			if _, err := fmt.Fprintf(w, "%s=%s\n", it.Name, it.Value); err != nil {
				return err
			}
		}
		return nil

	default:
		return fmt.Errorf("unsupported format %q", format)
	}
}
