package providers

import "encoding/json"

// decode parses JSON into generic Go types, normalizing json.Number to float64
// so token arithmetic uses plain numbers (mirroring JS JSON.parse).
func decode(data string) (interface{}, error) {
	var v interface{}
	if err := json.Unmarshal([]byte(data), &v); err != nil {
		return nil, err
	}
	return v, nil
}
