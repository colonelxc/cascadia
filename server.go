package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/net/html"
)

const (
	PORTAL_URL = "https://securelink.labmed.uw.edu/cascadia/result"
)

// Config struct

type ConfigPerson struct {
	Name        string `json:"name"`
	DateOfBirth string `json:"date_of_birth"` // MM/DD/YYYY
}

type Config struct {
	People       []ConfigPerson `json:"people"`
	DatabasePath string         `json:"database_path"`
}

// State

type server struct {
	db        *sql.DB
	config    Config
	indextmpl *template.Template
}

func (s *server) ConnectOrCreateSQL() {
	db, err := sql.Open("sqlite3", s.config.DatabasePath)
	if err != nil {
		log.Fatal(err)
	}
	s.db = db

	_, err = db.Exec("CREATE TABLE IF NOT EXISTS Samples (name text, barcode text, results text, created_time timestamp, updated_time timestamp, sample_date)")
	if err != nil {
		log.Fatal(err)
	}

	log.Print("DB Ready")
}

func (s *server) GetSamples(limit int) ([]Sample, error) {
	rows, err := s.db.Query("SELECT * FROM Samples ORDER BY updated_time DESC LIMIT ?", limit)
	if err != nil {
		return nil, err
	}

	samples := make([]Sample, 0)
	for rows.Next() {
		s := Sample{}
		err := rows.Scan(&s.Name, &s.Barcode, &s.Results, &s.CreatedTime, &s.UpdatedTime, &s.SampleDate)
		if err != nil {
			return nil, err
		}
		samples = append(samples, s)
	}
	log.Printf("Retrieved %d samples. %+v", len(samples), samples)
	return samples, nil
}

func (s *server) AddSample(name string, barcode string) error {
	// TODO: check to make sure name makes sense
	t := time.Now()
	_, err := s.db.Exec("INSERT INTO Samples VALUES (?, ?, 'pending', ?, ?, NULL)", name, barcode, &t, &t)

	return err
}

func (s *server) prepareTemplates() {
	s.indextmpl = template.Must(template.ParseFiles("index.tmpl.html"))
}

type Response struct {
	People  []ConfigPerson
	Samples []Sample
}

type Sample struct {
	Name        string
	Barcode     string
	Results     sql.NullString
	CreatedTime *time.Time
	UpdatedTime *time.Time
	SampleDate  sql.NullString
}

func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	log.Print("Received request")
	samples, err := s.GetSamples(10)
	if err != nil {
		log.Printf("Error serving request: %v", err)
		w.WriteHeader(500)
		return
	}
	s.indextmpl.Execute(w, Response{People: s.config.People, Samples: samples})
}

func (s *server) handleNewSample(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		http.NotFound(w, r)
		return
	}
	log.Print("New Barcode")
	err := r.ParseForm()
	if err != nil {
		log.Printf("Error serving request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	name := r.Form.Get("person")
	barcode := r.Form.Get("barcode")
	if name == "" || barcode == "" {
		log.Printf("Missing arguments, (%s, %s)", name, barcode)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	err = s.AddSample(name, barcode)
	if err != nil {
		log.Printf("add sample error: %v", err)
		w.WriteHeader(500)
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

var s *server

// Main that starts a server listening on localhost (maybe configurable)
func main() {
	s = &server{}
	f, err := os.Open("config.json")
	if err != nil {
		log.Fatal(err)
	}
	decoder := json.NewDecoder(f)
	decoder.Decode(&s.config)
	log.Printf("config: %+v", s.config)

	s.ConnectOrCreateSQL()
	defer s.db.Close()
	s.prepareTemplates()
	http.HandleFunc("/", s.handleIndex)
	http.HandleFunc("/new", s.handleNewSample)
	go s.periodicallyUpdate()
	log.Fatal(http.ListenAndServe("127.0.0.1:9000", nil))
}

// polling code

func (s *server) periodicallyUpdate() {
	s.updatePending()
	t := time.NewTicker(time.Hour * 12)
	for range t.C {
		s.updatePending()
	}
}

func (s *server) updatePending() {
	rows, err := s.db.Query("SELECT * FROM Samples WHERE results LIKE '%pending%'")
	if err != nil {
		log.Printf("Polling error: %v", err)
		return
	}

	samples := make([]Sample, 0)
	for rows.Next() {
		s := Sample{}
		err := rows.Scan(&s.Name, &s.Barcode, &s.Results, &s.CreatedTime, &s.UpdatedTime, &s.SampleDate)
		if err != nil {
			log.Printf("Polling error: %v", err)
			return
		}
		samples = append(samples, s)
	}
	log.Printf("Retrieved %d pending samples. %+v", len(samples), samples)

	for _, sample := range samples {
		s.updateOne(sample)
	}

}

func (s *server) updateOne(smpl Sample) {
	dob := ""
	for _, p := range s.config.People {
		if p.Name == smpl.Name {
			dob = p.DateOfBirth
			break
		}
	}
	if dob == "" {
		log.Printf("Couldn't find match for name: %s", smpl.Name)
		return
	}

	resp, err := http.PostForm(PORTAL_URL, url.Values{"barcode": []string{smpl.Barcode}, "dob": []string{dob}})
	if err != nil {
		log.Printf("Retrieve results error: %v", err)
		return
	}
	data, err := getAllTDs(resp.Body)
	if err != nil {
		fmt.Printf("parsing error: %v", err)
		return
	}
	resp.Body.Close()

	log.Printf("data: %+v", data)

	if len(data) == 0 {
		log.Printf("No data yet, skipping")
		return
	}
	if len(data) < 4 || (len(data) >= 5 && len(data)%2 != 1) {
		log.Printf("Uh, don't know what is happening here")
		return
	}
	results := data[1] + " " + data[2]
	for i := 3; i < len(data)-3; i = i + 2 { // Additional rows
		results = results + " | " + data[i] + " " + data[i+1]
	}
	t := time.Now()
	res, err := s.db.Exec("UPDATE Samples SET results = ?, updated_time = ?, sample_date = ? WHERE barcode = ?", results, &t, data[len(data)-2], smpl.Barcode)
	if err != nil {
		log.Printf("Error saving: %v", err)
		return
	}
	if num, _ := res.RowsAffected(); num != 1 {
		log.Fatalf("Expected to update one row, updated %d", num)
	}
}

// You can't parse html with regex! *shrug*

func getAllTDs(r io.Reader) ([]string, error) {
	h := html.NewTokenizer(r)
	data := []string{}
	grabNextText := false
	for {
		tokenType := h.Next()
		if tokenType == html.ErrorToken {
			err := h.Err()
			if err == io.EOF {
				return data, nil
			}
			return nil, err
		}

		token := h.Token()
		if tokenType == html.StartTagToken && token.Data == "td" {
			grabNextText = true
		}
		if tokenType == html.TextToken && grabNextText {
			d := strings.TrimSpace(token.Data)
			if d == "" {
				log.Printf("skipping...")
				continue
			}
			log.Printf("Found '%s' in the html!", d)
			data = append(data, d)
			grabNextText = false
		}
	}
}
