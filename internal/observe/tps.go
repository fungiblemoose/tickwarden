package observe

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// TPSReader returns the server's current ticks-per-second and mean tick time
// in milliseconds (MSPT). Implementations are pluggable so the correlation
// loop doesn't care where the numbers come from.
type TPSReader interface {
	Read() (tps float64, mspt float64, err error)
	Name() string
}

// SparkHTTPReader reads TPS/MSPT from a spark health endpoint exposed over
// HTTP. spark itself doesn't ship an HTTP server, so this expects a thin
// companion (e.g. a small Fabric mod or spark's JSON export served by a
// sidecar) at the given URL returning {"tps": <float>, "mspt": <float>}.
//
// This is the seam where the optional companion mod plugs in; until then the
// StubReader keeps the loop runnable for development.
type SparkHTTPReader struct {
	URL    string
	Client *http.Client
}

func NewSparkHTTPReader(url string) *SparkHTTPReader {
	return &SparkHTTPReader{
		URL:    url,
		Client: &http.Client{Timeout: 2 * time.Second},
	}
}

func (r *SparkHTTPReader) Name() string { return "spark-http(" + r.URL + ")" }

func (r *SparkHTTPReader) Read() (float64, float64, error) {
	resp, err := r.Client.Get(r.URL)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, 0, fmt.Errorf("tps endpoint returned %d", resp.StatusCode)
	}
	var payload struct {
		TPS  float64 `json:"tps"`
		MSPT float64 `json:"mspt"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, 0, err
	}
	return payload.TPS, payload.MSPT, nil
}

// FetchPlayerPeak reads the companion's observed peak concurrent player count
// from its endpoint. This is what lets `tune` size distances for the server's
// ACTUAL measured load instead of a guess — closing the loop from "configure
// for an assumed player count" to "configure for the one you really get".
func FetchPlayerPeak(url string) (int, error) {
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("endpoint returned %d", resp.StatusCode)
	}
	var payload struct {
		PlayersPeak *int `json:"players_peak"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, err
	}
	if payload.PlayersPeak == nil {
		return 0, fmt.Errorf("endpoint did not report players_peak (update the companion mod)")
	}
	return *payload.PlayersPeak, nil
}

// StubReader reports a flat healthy 20 TPS. Useful for exercising the watch
// loop and PSI correlation without a live server wired up.
type StubReader struct{}

func (StubReader) Name() string                    { return "stub" }
func (StubReader) Read() (float64, float64, error) { return 20.0, 50.0, nil }
