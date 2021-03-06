module github.com/VictoriaMetrics/VictoriaMetrics

require (
	cloud.google.com/go/storage v1.11.0
	github.com/VictoriaMetrics/fastcache v1.5.7

	// Do not use the original github.com/valyala/fasthttp because of issues
	// like https://github.com/valyala/fasthttp/commit/996610f021ff45fdc98c2ce7884d5fa4e7f9199b
	github.com/VictoriaMetrics/fasthttp v1.0.5
	github.com/VictoriaMetrics/metrics v1.12.3
	github.com/VictoriaMetrics/metricsql v0.6.0
	github.com/aws/aws-sdk-go v1.34.20
	github.com/cespare/xxhash/v2 v2.1.1
	github.com/golang/snappy v0.0.1
	github.com/klauspost/compress v1.11.0
	github.com/valyala/fastjson v1.5.4
	github.com/valyala/fastrand v1.0.0
	github.com/valyala/fasttemplate v1.2.1
	github.com/valyala/gozstd v1.8.3
	github.com/valyala/histogram v1.1.2
	github.com/valyala/quicktemplate v1.6.2
	golang.org/x/net v0.0.0-20200904194848-62affa334b73 // indirect
	golang.org/x/oauth2 v0.0.0-20200902213428-5d25da1a8d43
	golang.org/x/sys v0.0.0-20200909081042-eff7692f9009
	golang.org/x/tools v0.0.0-20200909210914-44a2922940c2 // indirect
	google.golang.org/api v0.31.0
	google.golang.org/genproto v0.0.0-20200904004341-0bd0a958aa1d // indirect
	google.golang.org/grpc v1.32.0 // indirect
	gopkg.in/yaml.v2 v2.3.0
)

go 1.13
