package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	neturl "net/url"
	"regexp"
	"strings"
	"time"

	"github.com/PuerkitoBio/goquery"
	"github.com/gorilla/mux"
)

type Competition struct {
	ID          string            `json:"id"`
	Code        string            `json:"code"`
	Name        string            `json:"name"`
	TeamCount   string            `json:"team_count"`
	MatchesLink string            `json:"matches_link"`
	Matches     []Match           `json:"matches,omitempty"`
	Table       *CompetitionTable `json:"table,omitempty"`
}

// --- Logo resolution via local /club/search with simple in-memory cache ---
var logoCache = map[string]string{}

type searchAPIResult struct {
	Results []struct {
		Name    string `json:"name"`
		LogoURL string `json:"logo_url"`
	} `json:"results"`
}

func getLogoBySearch(name string) string {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return ""
	}
	if v, ok := logoCache[key]; ok {
		return v
	}
	// Query local API
	apiURL := fmt.Sprintf("http://localhost:8080/club/search?q=%s", neturl.QueryEscape(name))
	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// drain body to allow reuse
		io.Copy(io.Discard, resp.Body)
		return ""
	}
	var payload searchAPIResult
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return ""
	}
	// pick best match: exact (case-insensitive), then contains, else first
	best := ""
	for _, r := range payload.Results {
		if strings.EqualFold(strings.TrimSpace(r.Name), strings.TrimSpace(name)) {
			best = r.LogoURL
			break
		}
	}
	if best == "" {
		for _, r := range payload.Results {
			if strings.Contains(strings.ToLower(r.Name), key) || strings.Contains(key, strings.ToLower(r.Name)) {
				best = r.LogoURL
				break
			}
		}
	}
	if best == "" && len(payload.Results) > 0 {
		best = payload.Results[0].LogoURL
	}
	logoCache[key] = best
	return best
}

func getLogo(teamName string, teamID string) string {
	placeholder := "https://www.fotbal.cz/dist/img/logo-club-empty.svg"
	name := strings.ToLower(strings.TrimSpace(teamName))
	if name == "" || strings.Contains(name, "volno") || strings.Contains(name, "volný los") || strings.Contains(name, "volny los") || strings.Contains(name, "bye") {
		return placeholder
	}
	// If we have a team ID, construct the official logo URL directly.
	// This avoids wrong matches for duplicate names (e.g., multiple "Ořechov").
	if tid := strings.TrimSpace(teamID); tid != "" {
		return fmt.Sprintf("https://is1.fotbal.cz/media/kluby/%s/%s_crop.jpg", tid, tid)
	}
	// Otherwise, try the local search endpoint by name.
	if logo := getLogoBySearch(teamName); logo != "" {
		return logo
	}
	// No ID and no search hit -> placeholder
	return placeholder
}

// CompetitionTable holds standings sections; currently only Overall is used
type CompetitionTable struct {
	Overall []TableRow `json:"overall"`
}

// ClubInfo is the response for club info and tables endpoints
type ClubInfo struct {
	Name           string        `json:"name"`
	ClubID         string        `json:"club_id"`
	ClubType       string        `json:"club_type"`
	ClubInternalID string        `json:"club_internal_id,omitempty"`
	URL            string        `json:"url,omitempty"`
	LogoURL        string        `json:"logo_url,omitempty"`
	Address        string        `json:"address,omitempty"`
	Category       string        `json:"category,omitempty"`
	Competitions   []Competition `json:"competitions"`
}

// SearchResult represents one club from fotbal.cz search
type SearchResult struct {
	Name     string `json:"name"`
	ClubID   string `json:"club_id"`
	ClubType string `json:"club_type"` // football or futsal
	URL      string `json:"url"`
	LogoURL  string `json:"logo_url"`
	Category string `json:"category,omitempty"`
	Address  string `json:"address,omitempty"`
}

// getClubSearch queries fotbal.cz club search and returns results with logo
func getClubSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		http.Error(w, "query parameter 'q' is required", http.StatusBadRequest)
		return
	}

	// Build search URL
	vals := neturl.Values{}
	vals.Set("q", q)
	searchURL := "https://www.fotbal.cz/club/hledej?" + vals.Encode()

	req, err := http.NewRequest("GET", searchURL, nil)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error creating request: %v", err), http.StatusInternalServerError)
		return
	}
	// Set headers to mimic a browser; fotbal.cz may 404 otherwise
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	req.Header.Set("Accept-Language", "cs-CZ,cs;q=0.9,en;q=0.8")
	req.Header.Set("Referer", "https://www.fotbal.cz/club/hledej")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error fetching search page: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// Retry once. If query has very short tokens, try quoting the whole query.
		resp.Body.Close()
		searchURL2 := searchURL
		tokens := strings.Fields(q)
		for _, t := range tokens {
			if len([]rune(t)) <= 2 {
				vals2 := neturl.Values{}
				vals2.Set("q", "\""+q+"\"")
				searchURL2 = "https://www.fotbal.cz/club/hledej?" + vals2.Encode()
				break
			}
		}
		req2, _ := http.NewRequest("GET", searchURL2, nil)
		req2.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36")
		req2.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req2.Header.Set("Accept-Language", "en-US,en;q=0.9")
		resp2, err2 := client.Do(req2)
		if err2 != nil {
			http.Error(w, fmt.Sprintf("Error fetching (retry): %v", err2), http.StatusBadGateway)
			return
		}
		defer resp2.Body.Close()
		if resp2.StatusCode != http.StatusOK {
			// Treat as no results instead of surfacing error to client
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"query":   q,
				"count":   0,
				"results": []SearchResult{},
			})
			return
		}
		// replace resp with resp2 for downstream parsing
		resp = resp2
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error parsing HTML: %v", err), http.StatusInternalServerError)
		return
	}

	var results []SearchResult
	// The page lists clubs in section "Výsledky hledání" as li.ListItemSplit
	doc.Find("li.ListItemSplit").Each(func(_ int, li *goquery.Selection) {
		a := li.Find("a.Link--inverted").First()
		href, _ := a.Attr("href")
		if href == "" {
			return
		}
		name := strings.TrimSpace(a.Find("span.H7").First().Text())
		if name == "" {
			// fallback to link text
			name = strings.TrimSpace(a.Text())
		}
		img := a.Find("img").First()
		logoURL, _ := img.Attr("src")

		// Category
		category := strings.TrimSpace(li.Find(".ClubCategories .BadgeCategory").First().Text())
		// Address
		address := strings.TrimSpace(li.Find(".ClubAddress p").First().Text())

		// Infer club type from href
		clubType := "football"
		if strings.Contains(strings.ToLower(href), "/futsal/") {
			clubType = "futsal"
		}

		// Extract club ID from last path segment
		// e.g., https://www.fotbal.cz/futsal/club/club/{uuid}
		parts := strings.Split(strings.TrimRight(href, "/"), "/")
		clubID := ""
		if len(parts) > 0 {
			clubID = parts[len(parts)-1]
		}

		// Normalize URL (ensure absolute)
		if !strings.HasPrefix(href, "http://") && !strings.HasPrefix(href, "https://") {
			href = "https://www.fotbal.cz" + href
		}

		results = append(results, SearchResult{
			Name:     name,
			ClubID:   clubID,
			ClubType: clubType,
			URL:      href,
			LogoURL:  logoURL,
			Category: category,
			Address:  address,
		})
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"query":   q,
		"count":   len(results),
		"results": results,
	})
}

// getClubTables returns club info with competition standings tables (no matches)
func getClubTables(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	clubID := vars["id"]
	clubType := vars["type"]

	if clubID == "" {
		http.Error(w, "Club ID is required", http.StatusBadRequest)
		return
	}

	// Validate club type
	var baseURL string
	var sportParam string
	switch clubType {
	case "football":
		baseURL = "https://www.fotbal.cz/souteze/club/club"
		sportParam = "fotbal"
	case "futsal":
		baseURL = "https://www.fotbal.cz/futsal/club/club"
		sportParam = "futsal"
	default:
		http.Error(w, "Invalid club type. Use 'football' or 'futsal'.", http.StatusBadRequest)
		return
	}

	url := fmt.Sprintf("%s/%s", baseURL, clubID)
	resp, err := http.Get(url)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error fetching club data: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("Error: received status code %d", resp.StatusCode), resp.StatusCode)
		return
	}

	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error parsing HTML: %v", err), http.StatusInternalServerError)
		return
	}
	// Extract club internal ID
	clubInternalID := ""
	doc.Find("section").Each(func(i int, s *goquery.Selection) {
		headerText := s.Find("h3 span").First().Text()
		if strings.TrimSpace(headerText) == "ID klubu" {
			clubInternalID = strings.TrimSpace(s.Find("ul li").First().Text())
		}
	})

	// Extract competitions
	var competitions []Competition
	doc.Find("table.Table tbody tr").Each(func(i int, s *goquery.Selection) {
		code := strings.TrimSpace(s.Find("td:first-child").Text())
		nameLink := s.Find("td:nth-child(2) a")
		name := strings.TrimSpace(nameLink.Text())
		teamCount := strings.TrimSpace(s.Find("td:nth-child(3)").Text())
		// Extract competition ID from the link
		parts := strings.Split(nameLink.AttrOr("href", ""), "/")
		compID := ""
		if len(parts) >= 2 {
			compID = parts[len(parts)-1]
		}
		// Build public table link depending on clubType
		tableLink := ""
		if strings.EqualFold(clubType, "futsal") {
			tableLink = fmt.Sprintf("https://www.fotbal.cz/futsal/futsal/table/%s", compID)
		} else {
			tableLink = fmt.Sprintf("https://www.fotbal.cz/souteze/turnaje/table/%s", compID)
		}

		competitions = append(competitions, Competition{
			ID:          compID,
			Code:        code,
			Name:        name,
			TeamCount:   teamCount,
			MatchesLink: tableLink,
		})
	})

	// For each competition, fetch the standings tables from is.fotbal.cz
	for i := range competitions {
		comp := &competitions[i]
		tableURL := fmt.Sprintf("https://is.fotbal.cz/public/souteze/tabulky-souteze.aspx?req=%s&sport=%s", comp.ID, sportParam)
		resp, err := http.Get(tableURL)
		if err != nil {
			log.Printf("error fetching competition table for %s: %v", comp.ID, err)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("non-200 response for %s: %d", comp.ID, resp.StatusCode)
			continue
		}

		docTable, err := goquery.NewDocumentFromReader(resp.Body)
		if err != nil {
			log.Printf("error parsing table HTML for %s: %v", comp.ID, err)
			continue
		}

		// Parse section: Tabulka celková (only overall)
		var overall []TableRow

		parseSection := func(headerText string) []TableRow {
			var rows []TableRow
			// Find the h3 with matching text, then the following .list.tabulky table
			docTable.Find("h3").EachWithBreak(func(_ int, h3 *goquery.Selection) bool {
				if strings.EqualFold(strings.TrimSpace(h3.Text()), headerText) {
					list := h3.NextAllFiltered("div.list.tabulky").First()
					if list.Length() == 0 {
						return false
					}
					table := list.Find("table.vysledky-tabulky tbody")
					table.Find("tr").Each(func(_ int, tr *goquery.Selection) {
						// skip header rows containing th
						if tr.Find("th").Length() > 0 {
							return
						}
						tds := tr.Find("td")
						if tds.Length() < 8 {
							return
						}
						get := func(i int) string { return strings.TrimSpace(tds.Eq(i).Text()) }
						rank := get(0)
						team := get(1)
						teamID := extractUUIDFromHref(tds.Eq(1).Find("a").First().AttrOr("href", ""))
						played := get(2)
						wins := get(3)
						draws := get(4)
						losses := get(5)
						scoreRaw := get(6)
						// normalize score like "5 : 0" -> "5:0"
						score := scoreRaw
						if re := regexp.MustCompile(`\s*([0-9]+)\s*:\s*([0-9]+)\s*`); re != nil {
							if m := re.FindStringSubmatch(scoreRaw); len(m) == 3 {
								score = fmt.Sprintf("%s:%s", m[1], m[2])
							}
						}
						points := get(7)
						rows = append(rows, TableRow{
							Rank: rank, Team: team, TeamID: teamID, TeamLogoURL: getLogo(team, teamID), Played: played, Wins: wins, Draws: draws, Losses: losses, Score: score, Points: points,
						})
					})
					return false
				}
				return true
			})
			return rows
		}

		overall = parseSection("Tabulka celková")
		comp.Table = &CompetitionTable{Overall: overall}
	}

	clubName := strings.TrimSpace(doc.Find("h1.H4 span").First().Text())
	clubURL := strings.TrimSpace(doc.Find("h1.H4 a").First().AttrOr("href", ""))
	logoURL := strings.TrimSpace(doc.Find("img.Logo").First().AttrOr("src", ""))
	category := strings.TrimSpace(doc.Find("section").First().Find("h3 span").First().Text())
	address := strings.TrimSpace(doc.Find("section").First().Find("ul li").First().Text())

	clubInfo := ClubInfo{
		Name:           clubName,
		ClubID:         clubID,
		ClubType:       clubType,
		ClubInternalID: clubInternalID,
		URL:            clubURL,
		LogoURL:        logoURL,
		Address:        address,
		Category:       category,
		Competitions:   competitions,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(clubInfo)
}

// getClubInfo returns club info with competitions and matches
func getClubInfo(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	clubID := vars["id"]
	clubType := vars["type"]
	if clubID == "" {
		http.Error(w, "Club ID is required", http.StatusBadRequest)
		return
	}
	var baseURL, sportParam string
	switch clubType {
	case "football":
		baseURL = "https://www.fotbal.cz/souteze/club/club"
		sportParam = "fotbal"
	case "futsal":
		baseURL = "https://www.fotbal.cz/futsal/club/club"
		sportParam = "futsal"
	default:
		http.Error(w, "Invalid club type. Use 'football' or 'futsal'.", http.StatusBadRequest)
		return
	}

	url := fmt.Sprintf("%s/%s", baseURL, clubID)
	resp, err := http.Get(url)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error fetching club data: %v", err), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("Error: received status code %d", resp.StatusCode), resp.StatusCode)
		return
	}
	doc, err := goquery.NewDocumentFromReader(resp.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("Error parsing HTML: %v", err), http.StatusInternalServerError)
		return
	}

	clubName := strings.TrimSpace(doc.Find("h1.H4 span").First().Text())
	// Basic club metadata
	clubURL := fmt.Sprintf("%s/%s", baseURL, clubID)
	logoURL := fmt.Sprintf("https://is1.fotbal.cz/media/kluby/%s/%s_crop.jpg", clubID, clubID)
	category := "Fotbal"
	if strings.EqualFold(clubType, "futsal") {
		category = "Futsal"
	}
	// Internal ID
	clubInternalID := ""
	doc.Find("section").Each(func(_ int, s *goquery.Selection) {
		if strings.TrimSpace(s.Find("h3 span").First().Text()) == "ID klubu" {
			clubInternalID = strings.TrimSpace(s.Find("ul li").First().Text())
		}
	})
	// Address (best-effort)
	address := strings.TrimSpace(doc.Find(".ClubAddress p").First().Text())

	// Competitions list
	var competitions []Competition
	doc.Find("table.Table tbody tr").Each(func(_ int, tr *goquery.Selection) {
		code := strings.TrimSpace(tr.Find("td:first-child").Text())
		nameLink := tr.Find("td:nth-child(2) a")
		name := strings.TrimSpace(nameLink.Text())
		teamCount := strings.TrimSpace(tr.Find("td:nth-child(3)").Text())
		parts := strings.Split(strings.TrimSpace(nameLink.AttrOr("href", "")), "/")
		compID := ""
		if len(parts) >= 2 {
			compID = parts[len(parts)-1]
		}
		// Public table URL for convenience
		tableLink := ""
		if strings.EqualFold(clubType, "futsal") {
			tableLink = fmt.Sprintf("https://www.fotbal.cz/futsal/futsal/table/%s", compID)
		} else {
			tableLink = fmt.Sprintf("https://www.fotbal.cz/souteze/turnaje/table/%s", compID)
		}
		competitions = append(competitions, Competition{ID: compID, Code: code, Name: name, TeamCount: teamCount, MatchesLink: tableLink})
	})

	// For each competition, fetch matches
	for i := range competitions {
		comp := &competitions[i]
		detailURL := fmt.Sprintf("https://is.fotbal.cz/public/souteze/detail-souteze.aspx?req=%s&sport=%s", comp.ID, sportParam)
		resp, err := http.Get(detailURL)
		if err != nil {
			log.Printf("error fetching competition detail for %s: %v", comp.ID, err)
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			log.Printf("non-200 response for %s: %d", comp.ID, resp.StatusCode)
			continue
		}
		docDetail, err := goquery.NewDocumentFromReader(resp.Body)
		if err != nil {
			log.Printf("error parsing competition HTML for %s: %v", comp.ID, err)
			continue
		}
		var matches []Match
		docDetail.Find("table.soutez-zapasy tr").Each(func(_ int, s *goquery.Selection) {
			if s.Find("th").Length() > 0 { // skip header
				return
			}
			tds := s.Find("td")
			if tds.Length() < 7 {
				return
			}
			getText := func(sel *goquery.Selection) string { return strings.TrimSpace(sel.Text()) }
			dt := getText(tds.Eq(0))
			rawHome := getText(tds.Eq(1))
			if idx := strings.Index(rawHome, "("); idx >= 0 {
				rawHome = strings.TrimSpace(rawHome[:idx])
			}
			rawAway := getText(tds.Eq(2))
			if idx := strings.Index(rawAway, "("); idx >= 0 {
				rawAway = strings.TrimSpace(rawAway[:idx])
			}
			homeID := extractUUIDFromHref(tds.Eq(1).Find("a").First().AttrOr("href", ""))
			awayID := extractUUIDFromHref(tds.Eq(2).Find("a").First().AttrOr("href", ""))
			rawScore := getText(tds.Eq(3))
			score := ""
			if re := regexp.MustCompile(`(\d+)\s*:\s*(\d+)`); re != nil {
				if m := re.FindStringSubmatch(rawScore); len(m) == 3 {
					score = fmt.Sprintf("%s:%s", m[1], m[2])
				}
			}
			venue := getText(tds.Eq(4))
			note := ""
			var reportURL, matchID string
			tds.Eq(6).Find("a").Each(func(_ int, a *goquery.Selection) {
				href := strings.TrimSpace(a.AttrOr("href", ""))
				if href == "" {
					return
				}
				if u, err := neturl.Parse(href); err == nil {
					if id := u.Query().Get("zapas"); id != "" {
						matchID = id
					}
				}
			})
			if matchID != "" {
				if strings.EqualFold(clubType, "futsal") {
					reportURL = fmt.Sprintf("https://www.fotbal.cz/futsal/zapasy/futsal/%s", matchID)
				} else {
					reportURL = fmt.Sprintf("https://www.fotbal.cz/souteze/zapasy/zapas/%s", matchID)
				}
			}
			if clubName != "" {
				involved := strings.EqualFold(rawHome, clubName) || strings.EqualFold(rawAway, clubName) ||
					containsFold(clubName, rawHome) || containsFold(clubName, rawAway) ||
					containsFold(rawHome, clubName) || containsFold(rawAway, clubName)
				if !involved {
					return
				}
			}
			// Backfill IDs for the current club if missing to ensure correct logo resolution
			if homeID == "" {
				if strings.EqualFold(rawHome, clubName) || containsFold(rawHome, clubName) || containsFold(clubName, rawHome) {
					homeID = clubID
				}
			}
			if awayID == "" {
				if strings.EqualFold(rawAway, clubName) || containsFold(rawAway, clubName) || containsFold(clubName, rawAway) {
					awayID = clubID
				}
			}
			homeLogo := getLogo(rawHome, homeID)
			awayLogo := getLogo(rawAway, awayID)
			matches = append(matches, Match{DateTime: dt, Home: rawHome, HomeID: homeID, HomeLogoURL: homeLogo, Away: rawAway, AwayID: awayID, AwayLogoURL: awayLogo, Score: score, Venue: venue, Note: note, MatchID: matchID, ReportURL: reportURL})
		})
		comp.Matches = matches
	}

	clubInfo := ClubInfo{
		Name:           clubName,
		ClubID:         clubID,
		ClubType:       clubType,
		ClubInternalID: clubInternalID,
		URL:            clubURL,
		LogoURL:        logoURL,
		Address:        address,
		Category:       category,
		Competitions:   competitions,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(clubInfo)
}

func main() {
    r := mux.NewRouter()
    r.HandleFunc("/club/{type}/{id}", getClubInfo).Methods("GET")
    r.HandleFunc("/club/{type}/{id}/table", getClubTables).Methods("GET")
    r.HandleFunc("/club/search", getClubSearch).Methods("GET")
    r.HandleFunc("/club/{id:[0-9a-fA-F-]+}", func(w http.ResponseWriter, r *http.Request) {
        vars := mux.Vars(r)
        http.Redirect(w, r, "/club/football/"+vars["id"], http.StatusMovedPermanently)
    }).Methods("GET")
    r.HandleFunc("/", docsHandler)
    port := ":8080"
    fmt.Printf("Server running on http://localhost%s\n", port)
    log.Fatal(http.ListenAndServe(port, r))
}

// docsHandler serves a simple HTML API documentation at the root endpoint.
func docsHandler(w http.ResponseWriter, r *http.Request) {
    w.Header().Set("Content-Type", "text/html; charset=utf-8")
    io.WriteString(w, `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width, initial-scale=1" />
  <title>FACR Scraper API Docs</title>
  <style>
    :root { color-scheme: light dark; }
    body { font-family: system-ui, -apple-system, Segoe UI, Roboto, Helvetica, Arial, sans-serif; margin: 0; padding: 24px; line-height: 1.5; }
    header { margin-bottom: 24px; }
    code, pre { background: rgba(127,127,127,.15); padding: .2em .4em; border-radius: 4px; }
    pre { padding: 12px; overflow: auto; }
    .ep { margin: 18px 0; padding: 16px; border-left: 4px solid #4f46e5; background: rgba(79,70,229,.08); border-radius: 6px; }
    h1 { margin: 0 0 8px; font-size: 1.6rem; }
    h2 { margin: 22px 0 8px; font-size: 1.2rem; }
    a { color: #2563eb; text-decoration: none; }
    a:hover { text-decoration: underline; }
    ul { padding-left: 18px; }
    footer { margin-top: 28px; font-size: .9rem; opacity: .8; }
  </style>
  <link rel="icon" href="data:," />
  <meta http-equiv="Cache-Control" content="no-store" />
  <meta name="robots" content="noindex" />
  <script>
    function ex(id, url) { const el = document.getElementById(id); el.textContent = window.location.origin + url; el.href = url; }
    window.addEventListener('DOMContentLoaded', ()=>{
      ex('ex-search', '/club/search?q=Sparta');
      ex('ex-info', '/club/football/00000000-0000-0000-0000-000000000000');
      ex('ex-table', '/club/football/00000000-0000-0000-0000-000000000000/table');
    });
  </script>
</head>
<body>
  <header>
    <h1>FACR Scraper API</h1>
    <p>Status: <code>ok</code> — server is running.</p>
  </header>

  <section class="ep">
    <h2>Search Clubs</h2>
    <p><strong>GET</strong> <code>/club/search?q=QUERY</code></p>
    <p>Find clubs on fotbal.cz. Supports football and futsal clubs.</p>
    <p>Example: <a id="ex-search" href="/club/search?q=Sparta">/club/search?q=Sparta</a></p>
    <details>
      <summary>Response shape</summary>
      <pre>{
  "query": "Sparta",
  "count": 2,
  "results": [
    {
      "name": "AC Sparta Praha",
      "club_id": "<uuid>",
      "club_type": "football",
      "url": "https://www.fotbal.cz/...",
      "logo_url": "https://.../logo.png",
      "category": "Muži",
      "address": "..."
    }
  ]
}</pre>
    </details>
  </section>

  <section class="ep">
    <h2>Club Info + Matches</h2>
    <p><strong>GET</strong> <code>/club/{type}/{id}</code></p>
    <ul>
      <li><code>{type}</code>: <code>football</code> | <code>futsal</code></li>
      <li><code>{id}</code>: club UUID from fotbal.cz</li>
    </ul>
    <p>Example: <a id="ex-info" href="/club/football/00000000-0000-0000-0000-000000000000">/club/football/{id}</a></p>
    <details>
      <summary>Response shape</summary>
      <pre>{
  "name": "AC Sparta Praha",
  "club_id": "00000000-0000-0000-0000-000000000000",
  "club_type": "football",
  "club_internal_id": "123456",
  "url": "https://www.fotbal.cz/...",
  "logo_url": "https://is1.fotbal.cz/media/kluby/.../logo.jpg",
  "address": "Milady Horákové 98, 160 00 Praha 6",
  "category": "Muži A",
  "competitions": [
    {
      "id": "12345",
      "code": "1. LIGA",
      "name": "Fortuna Liga",
      "team_count": "16",
      "matches_link": "https://www.fotbal.cz/...",
      "matches": [
        {
          "date_time": "12.08.2023 18:00",
          "home": "AC Sparta Praha",
          "home_id": "00000000-0000-0000-0000-000000000000",
          "home_logo_url": "https://.../sparta.png",
          "away": "SK Slavia Praha",
          "away_id": "11111111-1111-1111-1111-111111111111",
          "away_logo_url": "https://.../slavia.png",
          "score": "2:1",
          "venue": "Stadion Letná",
          "match_id": "match12345",
          "report_url": "https://www.fotbal.cz/..."
        }
      ]
    }
  ]
}</pre>
    </details>
  </section>

  <section class="ep">
    <h2>Club Tables (Standings)</h2>
    <p><strong>GET</strong> <code>/club/{type}/{id}/table</code></p>
    <p>Returns standings (overall table) for each competition of the club.</p>
    <p>Example: <a id="ex-table" href="/club/football/00000000-0000-0000-0000-000000000000/table">/club/football/{id}/table</a></p>
    <details>
      <summary>Response shape</summary>
      <pre>{
  "name": "AC Sparta Praha",
  "club_id": "00000000-0000-0000-0000-000000000000",
  "club_type": "football",
  "club_internal_id": "123456",
  "url": "https://www.fotbal.cz/...",
  "logo_url": "https://is1.fotbal.cz/media/kluby/.../logo.jpg",
  "competitions": [
    {
      "id": "12345",
      "code": "1. LIGA",
      "name": "Fortuna Liga",
      "team_count": "16",
      "matches_link": "https://www.fotbal.cz/...",
      "table": {
        "overall": [
          {
            "rank": "1",
            "team": "AC Sparta Praha",
            "team_id": "00000000-0000-0000-0000-000000000000",
            "team_logo_url": "https://.../sparta.png",
            "played": "10",
            "wins": "8",
            "draws": "2",
            "losses": "0",
            "score": "25:5",
            "points": "26"
          },
          {
            "rank": "2",
            "team": "SK Slavia Praha",
            "team_id": "11111111-1111-1111-1111-111111111111",
            "team_logo_url": "https://.../slavia.png",
            "played": "10",
            "wins": "7",
            "draws": "2",
            "losses": "1",
            "score": "20:8",
            "points": "23"
          }
        ]
      }
    }
  ]
}</pre>
    </details>
  </section>

  <section class="ep">
    <h2>Shortcuts</h2>
    <p><strong>GET</strong> <code>/club/{id}</code> → redirects to <code>/club/football/{id}</code></p>
  </section>

  <footer>
    <p>Tip: Use a reverse proxy in production and set proper timeouts. This API scrapes public pages and may be rate-limited upstream.</p>
  </footer>
</body>
</html>`)
}

func containsFold(s, substr string) bool {
    s = strings.ToLower(strings.TrimSpace(s))
    substr = strings.ToLower(strings.TrimSpace(substr))
    if substr == "" {
        return false
    }
    return strings.Contains(s, substr)
}

// extractUUIDFromHref finds the first UUID-like token in an href and returns it.
func extractUUIDFromHref(href string) string {
    href = strings.TrimSpace(href)
    if href == "" {
        return ""
    }
    re := regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
    if m := re.FindString(href); m != "" {
        return m
    }
    // Fallback: some links may end with ID after slash; take last path token if it looks like hex+hyphenated
    parts := strings.Split(href, "/")
    if len(parts) > 0 {
        cand := parts[len(parts)-1]
        if re.MatchString(cand) {
            return cand
        }
    }
    return ""
}

type Match struct {
	DateTime      string `json:"date_time"`
	Home          string `json:"home"`
	HomeID        string `json:"home_id,omitempty"`
	HomeLogoURL   string `json:"home_logo_url,omitempty"`
	Away          string `json:"away"`
	AwayID        string `json:"away_id,omitempty"`
	AwayLogoURL   string `json:"away_logo_url,omitempty"`
	Score         string `json:"score"`
	Venue         string `json:"venue"`
	Note          string `json:"note,omitempty"`
	MatchID       string `json:"match_id"`
	ReportURL     string `json:"report_url,omitempty"`
	DelegationURL string `json:"delegation_url,omitempty"`
}

// TableRow represents one row in a standings table
type TableRow struct {
	Rank        string `json:"rank"`
	Team        string `json:"team"`
	TeamID      string `json:"team_id,omitempty"`
	TeamLogoURL string `json:"team_logo_url,omitempty"`
	Played      string `json:"played"`
	Wins        string `json:"wins"`
	Draws       string `json:"draws"`
	Losses      string `json:"losses"`
	Score       string `json:"score"`
	Points      string `json:"points"`
}

// resolveISURL makes relative IS links absolute against https://is.fotbal.cz/public/
func resolveISURL(href string) string {
	href = strings.TrimSpace(href)
	if strings.HasPrefix(href, "http://") || strings.HasPrefix(href, "https://") {
		if u, err := neturl.Parse(href); err == nil {
			u.Scheme = "https"
			u.Host = "is.fotbal.cz"
			if !strings.HasPrefix(u.Path, "/public/") {
				if strings.HasPrefix(u.Path, "/zapasy/") {
					u.Path = "/public" + u.Path
				}
			}
			q := u.Query()
			q.Del("discipline")
			u.RawQuery = q.Encode()
			return u.String()
		}
		return href
	}
	href = strings.TrimPrefix(href, "./")
	for strings.HasPrefix(href, "../") {
		href = strings.TrimPrefix(href, "../")
	}
	if strings.HasPrefix(href, "/") {
		href = strings.TrimPrefix(href, "/")
	}
	path := "/public/" + href
	u := neturl.URL{Scheme: "https", Host: "is.fotbal.cz", Path: path}
	return u.String()
}
