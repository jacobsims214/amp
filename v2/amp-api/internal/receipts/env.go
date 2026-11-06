package receipts

import (
	"fmt"
	"strconv"
	"strings"
)

// ConfigFromEnv reads the store's settings (spec, "Config"):
// RECEIPT_STORE (local or s3; default local), RECEIPT_DIR for local, and for
// s3 RECEIPT_S3_ENDPOINT (host:port or a URL), RECEIPT_S3_BUCKET,
// RECEIPT_S3_ACCESS_KEY, RECEIPT_S3_SECRET_KEY, RECEIPT_S3_REGION (default
// us-east-1), RECEIPT_S3_USE_SSL (default true), RECEIPT_S3_PATH_STYLE
// (default true) and RECEIPT_S3_SPOOL_DIR (default the system temporary
// directory). getenv is os.Getenv in amp-api. A malformed boolean is an
// error, not a silent default: it would decide whether keys go over TLS.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{
		Backend: strings.ToLower(strings.TrimSpace(getenv("RECEIPT_STORE"))),
		Dir:     getenv("RECEIPT_DIR"),
	}
	if cfg.Backend != "s3" {
		return cfg, nil
	}
	useSSL, err := envBool(getenv, "RECEIPT_S3_USE_SSL", true)
	if err != nil {
		return cfg, err
	}
	pathStyle, err := envBool(getenv, "RECEIPT_S3_PATH_STYLE", true)
	if err != nil {
		return cfg, err
	}
	region := strings.TrimSpace(getenv("RECEIPT_S3_REGION"))
	if region == "" {
		region = DefaultS3Region
	}
	cfg.S3 = S3Config{
		Endpoint:  strings.TrimSpace(getenv("RECEIPT_S3_ENDPOINT")),
		Bucket:    strings.TrimSpace(getenv("RECEIPT_S3_BUCKET")),
		AccessKey: strings.TrimSpace(getenv("RECEIPT_S3_ACCESS_KEY")),
		SecretKey: strings.TrimSpace(getenv("RECEIPT_S3_SECRET_KEY")),
		Region:    region,
		UseSSL:    useSSL,
		PathStyle: pathStyle,
		SpoolDir:  strings.TrimSpace(getenv("RECEIPT_S3_SPOOL_DIR")),
	}
	return cfg, nil
}

func envBool(getenv func(string) string, name string, def bool) (bool, error) {
	v := strings.TrimSpace(getenv(name))
	if v == "" {
		return def, nil
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def, fmt.Errorf("receipt store s3: %s=%q isn't true or false", name, v)
	}
	return b, nil
}
