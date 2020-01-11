// Binary breathe reads air quality data from a PMS5003 chip, exporting the data over prometheus HTTP.
//
// PMS5003 datasheet: http://www.aqmd.gov/docs/default-source/aq-spec/resources-page/plantower-pms5003-manual_v2-3.pdf
//
// TODO:
//   * Reset the chip when it borks? Reopen the serial port for every read?
//   * Pull only when prometheus does an HTTP request?
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"io"
	"net/http"
	"text/template"

	"log"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/jacobsa/go-serial/serial"
)

const (
	magic1 = 0x42 // :)
	magic2 = 0x4d
)

var (
	portname = flag.String("portname", "", "filename of serial port")
	port     = flag.String("port", ":1971", "http port to listen on")

	pms_received_packets = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "pms_received_packets",
		},
	)

	pms_packet_checksum_errors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "pms_packet_checksum_errors",
		},
	)

	pms_skipped_bytes = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "pms_skipped_bytes",
		},
	)

	// https://cdn-shop.adafruit.com/product-files/3686/plantower-pms5003-manual_v2-3.pdf
	pms_particulate_matter_standard = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pms_particulate_matter_standard",
			Help: "Micrograms per cubic meter, standard particle",
		},
		[]string{"microns"},
	)

	// https://cdn-shop.adafruit.com/product-files/3686/plantower-pms5003-manual_v2-3.pdf
	pms_particulate_matter_environmental = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pms_particulate_matter_environmental",
			Help: "micrograms per cubic meter, adjusted for atmospheric environment",
		},
		[]string{"microns"},
	)

	// https://cdn-shop.adafruit.com/product-files/3686/plantower-pms5003-manual_v2-3.pdf
	pms_particle_counts = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "pms_particle_counts",
			Help: "Number of particles with diameter beyond given number of microns in 0.1L of air",
		},
		[]string{"microns_lower_bound"},
	)

	index = template.Must(template.New("index").Parse(
		`<!doctype html>
	 <title>PMS5003 Prometheus Exporter</title>
	 <h1>PMS5003 Prometheus Exporter</h1>
	 <a href="/metrics">Metrics</a>
	 <p>
	 <pre>portname={{.}}</pre>
	 `))
)

func main() {
	flag.Parse()
	log.Printf("PMS Prometheus Exporter starting on port %v and file %v\n", *port, *portname)
	go readPortForever()
	http.Handle("/metrics", promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		index.Execute(w, *portname)
	})
	http.ListenAndServe(*port, nil)
}

func readPortForever() {
	options := serial.OpenOptions{
		PortName:        *portname,
		BaudRate:        9600,
		DataBits:        8,
		StopBits:        1,
		MinimumReadSize: 1,
	}

	port, err := serial.Open(options)
	if err != nil {
		log.Fatalf("serial.Open: %v", err)
	}

	defer port.Close()

	for {
		log.Println("Attempting to read.")
		pms, err := readPMS(port)
		if err != nil {
			log.Printf("readPMS: %v\n", err)
			continue
		}
		log.Printf("pms = %+v\n", pms)
		if !pms.valid() {
			log.Println("pms is not valid. Ignoring...")
			continue
		}
		pms_received_packets.Inc()
		pms_particulate_matter_standard.WithLabelValues("1").Set(float64(pms.Pm10Std))
		pms_particulate_matter_standard.WithLabelValues("2.5").Set(float64(pms.Pm25Std))
		pms_particulate_matter_standard.WithLabelValues("10").Set(float64(pms.Pm100Std))
		pms_particulate_matter_environmental.WithLabelValues("1").Set(float64(pms.Pm10Env))
		pms_particulate_matter_environmental.WithLabelValues("2.5").Set(float64(pms.Pm25Env))
		pms_particulate_matter_environmental.WithLabelValues("10").Set(float64(pms.Pm100Env))
		pms_particle_counts.WithLabelValues("3").Set(float64(pms.Particles3um))
		pms_particle_counts.WithLabelValues("5").Set(float64(pms.Particles5um))
		pms_particle_counts.WithLabelValues("10").Set(float64(pms.Particles10um))
		pms_particle_counts.WithLabelValues("25").Set(float64(pms.Particles25um))
		pms_particle_counts.WithLabelValues("50").Set(float64(pms.Particles50um))
		pms_particle_counts.WithLabelValues("100").Set(float64(pms.Particles100um))
	}
}

// PMS5003 wraps an air quality packet, as documented in https://cdn-shop.adafruit.com/product-files/3686/plantower-pms5003-manual_v2-3.pdf
type PMS5003 struct {
	Length         uint16
	Pm10Std        uint16
	Pm25Std        uint16
	Pm100Std       uint16
	Pm10Env        uint16
	Pm25Env        uint16
	Pm100Env       uint16
	Particles3um   uint16
	Particles5um   uint16
	Particles10um  uint16
	Particles25um  uint16
	Particles50um  uint16
	Particles100um uint16
	Unused         uint16
	Checksum       uint16
}

func (p *PMS5003) valid() bool {
	if p.Length != 28 {
		return false
	}
	return true
}

func readPMS(r io.Reader) (*PMS5003, error) {
	if err := awaitMagic(r); err != nil {
		// Read errors are likely unrecoverable - just quit and restart.
		log.Fatalf("awaitMagic: %v", err)
	}
	buf := make([]byte, 30)
	n, err := io.ReadFull(r, buf)
	if err != nil {
		// Read errors are likely unrecoverable - just quit and restart.
		log.Fatalf("ReadFull: %v", err)
	}
	if n != 30 {
		return nil, fmt.Errorf("too few bytes read: want %d got %d", 30, n)
	}

	var sum uint16 = uint16(magic1) + uint16(magic2)
	for i := 0; i < 28; i++ {
		sum += uint16(buf[i])
	}

	var p PMS5003
	bufR := bytes.NewReader(buf)
	binary.Read(bufR, binary.BigEndian, &p)

	if sum != p.Checksum {
		// This error is recoverable
		pms_packet_checksum_errors.Inc()
		return nil, fmt.Errorf("checksum: got %v want %v", sum, p)
	}
	return &p, nil
}

func awaitMagic(r io.Reader) error {
	log.Println("Awaiting magic... ")
	var b1 byte
	b2, err := pop(r)
	if err != nil {
		return err
	}
	for {
		b1 = b2
		b2, err = pop(r)
		if err != nil {
			return err
		}
		if b1 == magic1 && b2 == magic2 {
			log.Println("found magic!")
			return nil
		}
		pms_skipped_bytes.Inc()
	}
}

func pop(r io.Reader) (byte, error) {
	b := make([]byte, 1)
	_, err := r.Read(b)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}
