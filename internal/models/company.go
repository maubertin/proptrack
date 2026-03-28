package models

// Company represents a shipping company or operator node in Dgraph.
type Company struct {
	UID             string    `json:"uid,omitempty"`
	DgraphType      []string  `json:"dgraph.type,omitempty"`
	Name            string    `json:"company.name,omitempty"`
	Country         string    `json:"company.country,omitempty"`
	Sanctioned      bool      `json:"company.sanctioned,omitempty"`
	OFACID          string    `json:"company.ofac_id,omitempty"`
	KnownAliases    string    `json:"company.known_aliases,omitempty"`
	OperatesVessels []*Vessel `json:"company.operates_vessels,omitempty"`
}
