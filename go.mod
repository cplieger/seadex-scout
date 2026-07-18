module github.com/cplieger/seadex-scout

go 1.26.5

require (
	// arrapi v1.7.0 is UNPUBLISHED: a next-tag placeholder above the
	// published v1.6.0, which lacks the consumed GetEpisodeFiles (see
	// go.work for the local dev resolution).
	github.com/cplieger/arrapi v1.7.0
	github.com/cplieger/atomicfile/v2 v2.1.3
	github.com/cplieger/health v1.3.0
	github.com/cplieger/httpx/v2 v2.6.0 // indirect
	github.com/cplieger/webhttp v1.7.0
)

require go.yaml.in/yaml/v3 v3.0.4

require github.com/cplieger/slogx v1.4.0

require pgregory.net/rapid v1.3.0

require (
	github.com/cplieger/envx/yamlenv v1.0.0
	github.com/cplieger/scheduler/v2 v2.0.0
)

require github.com/cplieger/httpx/v3 v3.0.0

require github.com/cplieger/jsonx v1.0.0

require github.com/cplieger/textsafe v1.0.0
