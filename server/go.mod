module github.com/guru-bharadwaj20/private-cloud/server

// Two things set the floor:
//   - http.Request.Pattern (Go 1.23) lets the metrics middleware label by route
//     pattern rather than raw path; raw paths would give every filename its own
//     time series and blow up Prometheus cardinality.
//   - go-webauthn requires Go >= 1.25. Pinning a security-critical auth library
//     to an older release to avoid a toolchain bump is the wrong trade.
go 1.25.0

require (
	github.com/coreos/go-oidc/v3 v3.20.0
	github.com/go-webauthn/webauthn v0.17.4
	github.com/google/uuid v1.6.0
	github.com/jackc/pgx/v5 v5.9.2
	github.com/klauspost/compress v1.19.1
	github.com/ledongthuc/pdf v0.0.0-20250511090121-5959a4027728
	github.com/pressly/goose/v3 v3.24.1
	github.com/prometheus/client_golang v1.20.5
	github.com/tigerwill90/fastcdc v1.2.2
	github.com/zeebo/blake3 v0.2.4
	golang.org/x/crypto v0.54.0
	golang.org/x/image v0.44.0
	golang.org/x/net v0.57.0
	golang.org/x/oauth2 v0.36.0
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/fxamacker/cbor/v2 v2.9.2 // indirect
	github.com/go-jose/go-jose/v4 v4.1.4 // indirect
	github.com/go-viper/mapstructure/v2 v2.5.0 // indirect
	github.com/go-webauthn/x v0.2.6 // indirect
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/google/go-tpm v0.9.8 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/klauspost/cpuid/v2 v2.0.12 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/philhofer/fwd v1.2.0 // indirect
	github.com/prometheus/client_model v0.6.1 // indirect
	github.com/prometheus/common v0.55.0 // indirect
	github.com/prometheus/procfs v0.15.1 // indirect
	github.com/sethvargo/go-retry v0.3.0 // indirect
	github.com/tinylib/msgp v1.6.4 // indirect
	github.com/x448/float16 v0.8.4 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/protobuf v1.34.2 // indirect
)
