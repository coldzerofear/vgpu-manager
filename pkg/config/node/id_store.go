package node

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/klog/v2"
)

type IDStore struct {
	sets.Set[string]
}

func (s IDStore) InsertStringID(ids string) IDStore {
	s.Insert(ids)
	return s
}

func (s IDStore) InsertIntID(ids ...int) IDStore {
	for _, id := range ids {
		s.Insert(strconv.Itoa(id))
	}
	return s
}

func (s *IDStore) HasStringID(id string) bool {
	return s.Has(id)
}

func (s *IDStore) HasIntID(id int) bool {
	return s.Has(strconv.Itoa(id))
}

func NewIntIDStore(ids ...int) IDStore {
	store := NewIDStore()
	store.InsertIntID(ids...)
	return store
}

func NewIDStore(ids ...string) IDStore {
	return IDStore{
		Set: sets.New[string](ids...),
	}
}

func (s *IDStore) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		return nil
	}
	var intIDs []int
	if err := json.Unmarshal(data, &intIDs); err == nil {
		store := NewIntIDStore(intIDs...)
		s.Set = store.Set
		return nil
	}
	var strIDs []string
	if err := json.Unmarshal(data, &strIDs); err == nil {
		store := NewIDStore(strIDs...)
		s.Set = store.Set
		return nil
	}
	store := parseDeviceIDs(string(data))
	s.Set = store.Set
	return nil
}

func (s IDStore) MarshalJSON() ([]byte, error) {
	ids := s.UnsortedList()
	slices.Sort(ids)
	return json.Marshal(ids)
}

func (s *IDStore) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var intIDs []int
	if err := unmarshal(&intIDs); err == nil {
		store := NewIntIDStore(intIDs...)
		s.Set = store.Set
		return nil
	}
	var strIDs []string
	if err := unmarshal(&strIDs); err == nil {
		store := NewIDStore(strIDs...)
		s.Set = store.Set
		return nil
	}
	var rawStr string
	if err := unmarshal(&rawStr); err == nil {
		store := parseDeviceIDs(rawStr)
		s.Set = store.Set
	}
	return nil
}

func (s IDStore) MarshalYAML() (interface{}, error) {
	ids := s.UnsortedList()
	slices.Sort(ids)
	return ids, nil
}

func parseDeviceIDs(deviceIDStr string) IDStore {
	store := NewIDStore()
	deviceIDStr = strings.TrimSpace(deviceIDStr)
	deviceIDStr = strings.Trim(deviceIDStr, "\"")
	deviceIDStr = strings.TrimSpace(deviceIDStr)
	if len(deviceIDStr) == 0 {
		return store
	}
	for _, str := range strings.Split(deviceIDStr, ",") {
		split := strings.Split(strings.TrimSpace(str), "..")
		switch len(split) {
		case 1:
			id := strings.TrimSpace(split[0])
			if len(id) > 0 {
				store.InsertStringID(id)
			}
		case 2:
			start, err := strconv.Atoi(strings.TrimSpace(split[0]))
			if err != nil {
				klog.ErrorS(err, "conversion split[0] to int type failed", "idStr", deviceIDStr, "split", split)
				continue
			}
			end, err := strconv.Atoi(strings.TrimSpace(split[1]))
			if err != nil {
				klog.ErrorS(err, "conversion split[1] to int type failed", "idStr", deviceIDStr, "split", split)
				continue
			}
			for ; start <= end; start++ {
				store.InsertIntID(start)
			}
		}
	}
	return store
}
