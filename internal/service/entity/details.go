// Copyright 2023 MIMIRO AS
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package entity

import (
	"encoding/binary"
	"encoding/json"
	"fmt"

	"github.com/dgraph-io/badger/v4"

	"github.com/mimiro-io/datahub/internal/service/namespace"
	"github.com/mimiro-io/datahub/internal/service/store"
	"github.com/mimiro-io/datahub/internal/service/types"
)

type Lookup struct {
	badger     store.BadgerStore
	namespaces namespace.Manager
}

func NewLookup(s store.BadgerStore) (Lookup, error) {
	ns := namespace.NewManager(s)
	return Lookup{s, ns}, nil
}

// Details retrieves a nested map structure with information about all datasets that contain entities with the given entity ID
// The optional datasetNames parameter allows to narrow down in which datasets the function searches
//
// # The result map has the following shape
//
//	{
//	    "dataset1": {
//	        "changes": [
//	            {"id":"ns3:3","internalId":8,"recorded":1662648998417816245,"refs":{},"props":{"ns3:name":"Frank"}}
//	        ],
//	        "latest": {"id":"ns3:3","internalId":8,"recorded":1662648998417816245,"refs":{},"props":{"ns3:name":"Frank"}}
//	    },
//	    "dataset2": {
//	        "changes": [
//	            {"id":"ns3:3","internalId":8,"recorded":1663074960494865060,"refs":{},"props":{"ns3:name":"Frank"}},
//	            {"id":"ns3:3","internalId":8,"recorded":1663075373488961084,"refs":{},"props":{"ns3:name":"Frank","ns4:extra":{"refs":{},"props":{}}}}
//	        ],
//	        "latest": {"id":"ns3:3","internalId":8,"recorded":1663075373488961084,"refs":{},"props":{"ns3:name":"Frank","ns4:extra":{"refs":{},"props":{}}}}
//	    },
//	}
func (l Lookup) Details(id string, datasetNames []string) (map[string]interface{}, error) {
	curie, err := l.asCURIE(id)
	if err != nil {
		return nil, err
	}
	b := l.badger.GetDB()

	rtxn := b.NewTransaction(false)
	defer rtxn.Discard()
	internalID, err := l.InternalIDForCURIE(rtxn, curie)
	if err != nil {
		return nil, err
	}

	scope := l.badger.LookupDatasetIDs(datasetNames)
	details, err := l.loadDetails(rtxn, internalID, scope)
	if err != nil {
		return nil, err
	}
	return details, nil
}

func (l Lookup) loadDetails(
	rtxn *badger.Txn,
	internalEntityID types.InternalID,
	scope []types.InternalDatasetID,
) (map[string]interface{}, error) {
	result := map[string]interface{}{}

	entityLocatorPrefixBuffer := store.SeekEntity(internalEntityID)
	opts1 := badger.DefaultIteratorOptions
	opts1.PrefetchValues = false
	opts1.Prefix = entityLocatorPrefixBuffer
	entityLocatorIterator := rtxn.NewIterator(opts1)
	defer entityLocatorIterator.Close()

	var prevValueBytes []byte
	var previousDatasetID types.InternalDatasetID = 0
	var currentDatasetID types.InternalDatasetID = 0
	partials := map[types.InternalDatasetID][]byte{}
	for entityLocatorIterator.Seek(entityLocatorPrefixBuffer); entityLocatorIterator.ValidForPrefix(entityLocatorPrefixBuffer); entityLocatorIterator.Next() {
		item := entityLocatorIterator.Item()
		key := item.Key()

		currentDatasetID = types.InternalDatasetID(binary.BigEndian.Uint32(key[10:]))

		// check if dataset has been deleted, or must be excluded
		datasetDeleted := l.badger.IsDatasetDeleted(currentDatasetID)
		datasetIncluded := len(scope) == 0 // no specified datasets means no restriction - all datasets are allowed
		if !datasetIncluded {
			for _, id := range scope {
				if id == currentDatasetID {
					datasetIncluded = true
					break
				}
			}
		}
		if datasetDeleted || !datasetIncluded {
			continue
		}

		if previousDatasetID != 0 {
			if currentDatasetID != previousDatasetID {
				partials[previousDatasetID] = prevValueBytes
			}
		}

		previousDatasetID = currentDatasetID

		// fixme: pre alloc big ish buffer once and use value size
		prevValueBytes, _ = item.ValueCopy(nil)
	}

	if previousDatasetID != 0 {
		partials[previousDatasetID] = prevValueBytes
	}

	for internalDatasetID, entityBytes := range partials {
		n, ok := l.badger.LookupDatasetName(internalDatasetID)
		if !ok {
			result[fmt.Sprintf("%v", internalDatasetID)] = "UNEXPECTED: dataset name not found"
		} else {
			var entity map[string]interface{}
			err := json.Unmarshal(entityBytes, &entity)
			if err != nil {
				return nil, err
			}
			changes, err := l.loadChanges(rtxn, internalEntityID, internalDatasetID)
			if err != nil {
				return nil, err
			}
			result[n] = map[string]interface{}{
				"latest":  entity,
				"changes": changes,
			}
		}
	}
	return result, nil
}

func (l Lookup) loadChanges(
	rtxn *badger.Txn,
	internalEntityID types.InternalID,
	internalDatasetID types.InternalDatasetID,
) ([]map[string]interface{}, error) {
	seekPrefix := store.SeekEntityChanges(internalDatasetID, internalEntityID)
	iteratorOptions := badger.DefaultIteratorOptions
	iteratorOptions.PrefetchValues = true
	iteratorOptions.PrefetchSize = 1
	iteratorOptions.Prefix = seekPrefix
	entityChangesIterator := rtxn.NewIterator(iteratorOptions)
	defer entityChangesIterator.Close()

	result := make([]map[string]interface{}, 0)
	for entityChangesIterator.Rewind(); entityChangesIterator.ValidForPrefix(seekPrefix); entityChangesIterator.Next() {
		item := entityChangesIterator.Item()
		change, _ := item.ValueCopy(nil)
		var entity map[string]interface{}
		err := json.Unmarshal(change, &entity)
		if err != nil {
			return nil, err
		}
		result = append(result, entity)
	}
	return result, nil
}
