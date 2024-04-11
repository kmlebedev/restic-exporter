package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"
)

type resticData struct {
	Stats     resticStatsData
	Snapshots []resticSnapshotData
}

type resticStatsData struct {
	TotalSize      int `json:"total_size"`
	TotalFileCount int `json:"total_file_count"`
}

type resticSnapshotData struct {
	Time     time.Time `json:"time"`
	Parent   string    `json:"parent"`
	Tree     string    `json:"tree"`
	Paths    []string  `json:"paths"`
	Tags     []string  `json:"tags"`
	Hostname string    `json:"hostname"`
	Username string    `json:"username"`
	ID       string    `json:"id"`
	ShortID  string    `json:"short_id"`
}

var (
	envResticBin = getEnvNotEmpty("RESTIC_EXPORTER_BIN")
	envPort      = getEnvNotEmpty("RESTIC_EXPORTER_PORT")
	envAddress   = getEnvNotEmpty("RESTIC_EXPORTER_ADDRESS")
	envCacheDir  = getEnvNotEmpty("RESTIC_EXPORTER_CACHEDIR")
)

func getEnvNotEmpty(name string) string {
	if val := os.Getenv(name); len(val) > 0 {
		return val
	}
	panic(name + " not set")
}

func main() {

	log.Println("Starting exporter on http://" + envAddress + ":" + envPort + " ...")

	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/probe", func(w http.ResponseWriter, req *http.Request) {
		probeHandler(w, req)
	})

	log.Fatal(http.ListenAndServe(envAddress+":"+envPort, nil))
}

func probeHandler(w http.ResponseWriter, r *http.Request) {

	var (
		snapshots_latest_time = prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "restic",
				Subsystem: "snapshots",
				Name:      "latest_time",
				Help:      "Time of the latest snapshot",
			},
			[]string{"hostname", "paths", "tags"},
		)
		latest_total_nfiles = prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "restic",
				Subsystem: "stats",
				Name:      "latest_total_nfiles",
				Help:      "Number of files",
			},
			[]string{"hostname", "paths", "tags"},
		)

		latest_total_size = prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Namespace: "restic",
				Subsystem: "stats",
				Name:      "latest_total_size",
				Help:      "Total Size",
			},
			[]string{"hostname", "paths", "tags"},
		)
		locks_latest_time = prometheus.NewGauge(
			prometheus.GaugeOpts{
				Namespace: "restic",
				Subsystem: "locks",
				Name:      "latest_time",
				Help:      "Time of the latest lock",
			},
		)
	)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	r = r.WithContext(ctx)

	// get ?target=<ip> parameter from request
	target := r.URL.Query().Get("target")
	tags := r.URL.Query().Get("tags")
	path := r.URL.Query().Get("path")
	if target == "" && tags == "" && path == "" {
		http.Error(w, "Target parameter is missing", http.StatusBadRequest)
		return
	}

	// create registry containing metrics
	registry := prometheus.NewPedanticRegistry()

	// add metrics to registry
	registry.MustRegister(latest_total_size)
	registry.MustRegister(latest_total_nfiles)
	registry.MustRegister(snapshots_latest_time)

	baseArgs := []string{"--cache-dir", envCacheDir, "--json", "--no-lock"}
	var args []string
	if target != "" {
		args = append(baseArgs, "--host", target)
	}
	if path != "" {
		args = append(baseArgs, "--path", path)
	}
	if tags != "" {
		for _, tag := range strings.Split(tags, ",") {
			args = append(baseArgs, "--tag", tag)
		}
	}
	resticLocksCmd := exec.Command(envResticBin, append([]string{"list", "locks"}, baseArgs...)...)
	resticStatsCmd := exec.Command(envResticBin, append([]string{"stats", "latest"}, args...)...)
	resticSnapshotsCmd := exec.Command(envResticBin, append([]string{"snapshots", "latest"}, args...)...)

	var rd resticData
	if stdOut, err := stdOutFromCmd(resticLocksCmd); err != nil {
		log.Println(err)
		return
	} else if len(stdOut.Bytes()) > 0 {
		locks_latest_time.SetToCurrentTime()
	}
	if err := unmarshallFromCmd(resticStatsCmd, &rd.Stats); err != nil {
		log.Println(err)
		return
	}

	if err := unmarshallFromCmd(resticSnapshotsCmd, &rd.Snapshots); err != nil {
		log.Println(err)
		return
	}

	if len(rd.Snapshots) != 0 {

		common_labels := prometheus.Labels{
			"hostname": rd.Snapshots[0].Hostname,
			"paths":    strings.Join(rd.Snapshots[0].Paths, ":"),
			"tags":     strings.Join(rd.Snapshots[0].Tags, ","),
		}

		// set metrics
		latest_total_size.With(common_labels).Set(float64(rd.Stats.TotalSize))
		latest_total_nfiles.With(common_labels).Set(float64(rd.Stats.TotalFileCount))
		snapshots_latest_time.With(common_labels).Set(float64(rd.Snapshots[0].Time.Unix()))
	}

	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)

}

func stdOutFromCmd(cmd *exec.Cmd) (*bytes.Buffer, error) {
	var (
		stdOut bytes.Buffer
		stdErr bytes.Buffer
		err    error
	)

	cmd.Stdout = &stdOut
	cmd.Stderr = &stdErr

	err = cmd.Run()
	if err != nil {
		log.Printf("Error occured while running '%s': %s\n", cmd.String(), stdErr.String())
		return nil, err
	}
	return &stdOut, nil
}

func unmarshallFromCmd(cmd *exec.Cmd, out interface{}) error {
	stdOut, err := stdOutFromCmd(cmd)
	if err != nil {
		return err
	}

	if err := json.Unmarshal(stdOut.Bytes(), &out); err != nil {
		return err
	}

	return nil
}
