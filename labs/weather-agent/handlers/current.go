// Package handlers holds the weather agent's Go handlers. Each registers
// itself by name in init; agent.yaml refers to it with `go: NAME`.
package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/jefflaplante/agentbus/a2a"
	"github.com/jefflaplante/agentbus/agent"
	"github.com/jefflaplante/agentbus/client"
)

func init() {
	agent.Register("current", current)
}

// The location comes from another agent: accept a plain place name only.
var locationRE = regexp.MustCompile(`^[A-Za-z][A-Za-z .,'-]{0,99}$`)

// current answers {"op": "current", "location": "City[, Region]"} with the
// current conditions from Open-Meteo. ctx ends at the request's deadline or
// the handler's timeout; every HTTP call uses it.
func current(ctx context.Context, e *a2a.Envelope) (any, error) {
	var body struct {
		Location string `json:"location"`
	}
	if err := e.DecodeBody(&body); err != nil || !locationRE.MatchString(strings.TrimSpace(body.Location)) {
		return nil, client.Errorf("invalid", `body must be {"op": "current", "location": "City, Region"}`)
	}
	city, region, _ := strings.Cut(body.Location, ",")
	region = strings.ToLower(strings.TrimSpace(region))

	var geo struct {
		Results []struct {
			Name        string  `json:"name"`
			Admin1      string  `json:"admin1"`
			CountryCode string  `json:"country_code"`
			Country     string  `json:"country"`
			Latitude    float64 `json:"latitude"`
			Longitude   float64 `json:"longitude"`
		} `json:"results"`
	}
	q := url.Values{"name": {strings.TrimSpace(city)}, "count": {"10"}, "language": {"en"}, "format": {"json"}}
	if err := getJSON(ctx, "https://geocoding-api.open-meteo.com/v1/search?"+q.Encode(), &geo); err != nil {
		return nil, client.Errorf("upstream_error", "geocoding failed: %v", err)
	}
	for _, r := range geo.Results {
		if region != "" && !strings.EqualFold(r.Admin1, region) && !strings.EqualFold(r.Country, region) &&
			!strings.EqualFold(r.CountryCode, region) && !strings.EqualFold(usStates[strings.ToUpper(region)], r.Admin1) {
			continue
		}
		var w struct {
			Current struct {
				Time        string  `json:"time"`
				Temperature float64 `json:"temperature_2m"`
				Wind        float64 `json:"wind_speed_10m"`
				Code        int     `json:"weather_code"`
			} `json:"current"`
		}
		wq := url.Values{
			"latitude": {fmt.Sprint(r.Latitude)}, "longitude": {fmt.Sprint(r.Longitude)},
			"current":          {"temperature_2m,wind_speed_10m,weather_code"},
			"temperature_unit": {"fahrenheit"}, "wind_speed_unit": {"mph"}, "timezone": {"auto"},
		}
		if err := getJSON(ctx, "https://api.open-meteo.com/v1/forecast?"+wq.Encode(), &w); err != nil {
			return nil, client.Errorf("upstream_error", "weather lookup failed: %v", err)
		}
		return map[string]any{
			"location":      strings.Join([]string{r.Name, r.Admin1, r.CountryCode}, ", "),
			"time":          w.Current.Time,
			"temperature_f": w.Current.Temperature,
			"wind_mph":      w.Current.Wind,
			"conditions":    wmo[w.Current.Code],
			"source":        "Open-Meteo (open-meteo.com)",
		}, nil
	}
	return nil, client.Errorf("not_found", "no place called %q", body.Location)
}

func getJSON(ctx context.Context, u string, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "agentbus-weather-lab/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(resp.Body).Decode(v)
}

var wmo = map[int]string{
	0: "Clear", 1: "Mainly clear", 2: "Partly cloudy", 3: "Overcast", 45: "Fog", 48: "Freezing fog",
	51: "Light drizzle", 53: "Drizzle", 55: "Heavy drizzle", 61: "Light rain", 63: "Rain", 65: "Heavy rain",
	71: "Light snow", 73: "Snow", 75: "Heavy snow", 80: "Rain showers", 81: "Rain showers",
	82: "Violent rain showers", 85: "Snow showers", 86: "Heavy snow showers", 95: "Thunderstorm",
}

var usStates = map[string]string{
	"AL": "Alabama", "AK": "Alaska", "AZ": "Arizona", "AR": "Arkansas", "CA": "California", "CO": "Colorado",
	"CT": "Connecticut", "DE": "Delaware", "DC": "District of Columbia", "FL": "Florida", "GA": "Georgia",
	"HI": "Hawaii", "ID": "Idaho", "IL": "Illinois", "IN": "Indiana", "IA": "Iowa", "KS": "Kansas",
	"KY": "Kentucky", "LA": "Louisiana", "ME": "Maine", "MD": "Maryland", "MA": "Massachusetts",
	"MI": "Michigan", "MN": "Minnesota", "MS": "Mississippi", "MO": "Missouri", "MT": "Montana",
	"NE": "Nebraska", "NV": "Nevada", "NH": "New Hampshire", "NJ": "New Jersey", "NM": "New Mexico",
	"NY": "New York", "NC": "North Carolina", "ND": "North Dakota", "OH": "Ohio", "OK": "Oklahoma",
	"OR": "Oregon", "PA": "Pennsylvania", "RI": "Rhode Island", "SC": "South Carolina", "SD": "South Dakota",
	"TN": "Tennessee", "TX": "Texas", "UT": "Utah", "VT": "Vermont", "VA": "Virginia", "WA": "Washington",
	"WV": "West Virginia", "WI": "Wisconsin", "WY": "Wyoming",
}
