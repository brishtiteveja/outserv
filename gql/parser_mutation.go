// Portions Copyright 2017-2018 Dgraph Labs, Inc. are available under the Apache 2.0 license.
// Portions Copyright 2022 Outcaste, Inc. are available under the Smart License.

package gql

import (
	"github.com/outcaste-io/outserv/lex"
	"github.com/outcaste-io/outserv/protos/pb"
)

// ParseMutation parses a block into a mutation. Returns an object with a mutation or
// an upsert block with mutation, otherwise returns nil with an error.
func ParseMutation(mutation string) (req *pb.Request, err error) {
	var lexer lex.Lexer
	lexer.Reset(mutation)
	lexer.Run(lexIdentifyBlock)
	if err := lexer.ValidateResult(); err != nil {
		return nil, err
	}

	it := lexer.NewIterator()
	if !it.Next() {
		return nil, it.Errorf("Invalid mutation")
	}

	item := it.Item()
	switch item.Typ {
	case itemUpsertBlock:
		if req, err = parseUpsertBlock(it); err != nil {
			return nil, err
		}
	case itemLeftCurl:
		mu, err := parseMutationBlock(it)
		if err != nil {
			return nil, err
		}
		req = &pb.Request{Mutations: []*pb.Mutation{mu}}
	default:
		return nil, it.Errorf("Unexpected token: [%s]", item.Val)
	}

	// mutations must be enclosed in a single block.
	if it.Next() && it.Item().Typ != lex.ItemEOF {
		return nil, it.Errorf("Unexpected %s after the end of the block", it.Item().Val)
	}

	return req, nil
}

// parseUpsertBlock parses the upsert block
func parseUpsertBlock(it *lex.ItemIterator) (*pb.Request, error) {
	var req *pb.Request
	var queryText, condText string
	var queryFound bool

	// ===>upsert<=== {...}
	if !it.Next() {
		return nil, it.Errorf("Unexpected end of upsert block")
	}

	// upsert ===>{<=== ....}
	item := it.Item()
	if item.Typ != itemLeftCurl {
		return nil, it.Errorf("Expected { at the start of block. Got: [%s]", item.Val)
	}

	for it.Next() {
		item = it.Item()
		switch {
		// upsert {... ===>}<===
		case item.Typ == itemRightCurl:
			switch {
			case req == nil:
				return nil, it.Errorf("Empty mutation block")
			case !queryFound:
				return nil, it.Errorf("Query op not found in upsert block")
			default:
				req.Query = queryText
				return req, nil
			}

		// upsert { mutation{...} ===>query<==={...}}
		case item.Typ == itemUpsertBlockOp && item.Val == "query":
			if queryFound {
				return nil, it.Errorf("Multiple query ops inside upsert block")
			}
			queryFound = true
			if !it.Next() {
				return nil, it.Errorf("Unexpected end of upsert block")
			}
			item = it.Item()
			if item.Typ != itemUpsertBlockOpContent {
				return nil, it.Errorf("Expecting brace, found '%s'", item.Val)
			}
			queryText += item.Val

		// upsert { ===>mutation<=== {...} query{...}}
		case item.Typ == itemUpsertBlockOp && item.Val == "mutation":
			if !it.Next() {
				return nil, it.Errorf("Unexpected end of upsert block")
			}

			// upsert { mutation ===>@if(...)<=== {....} query{...}}
			item = it.Item()
			if item.Typ == itemUpsertBlockOpContent {
				condText = item.Val
				if !it.Next() {
					return nil, it.Errorf("Unexpected end of upsert block")
				}
			}

			// upsert @if(...) ===>{<=== ....}
			mu, err := parseMutationBlock(it)
			if err != nil {
				return nil, err
			}
			mu.Cond = condText
			if req == nil {
				req = &pb.Request{Mutations: []*pb.Mutation{mu}}
			} else {
				req.Mutations = append(req.Mutations, mu)
			}

		// upsert { mutation{...} ===>fragment<==={...}}
		case item.Typ == itemUpsertBlockOp && item.Val == "fragment":
			if !it.Next() {
				return nil, it.Errorf("Unexpected end of upsert block")
			}
			item = it.Item()
			if item.Typ != itemUpsertBlockOpContent {
				return nil, it.Errorf("Expecting brace, found '%s'", item.Val)
			}
			queryText += "fragment" + item.Val

		default:
			return nil, it.Errorf("Unexpected token in upsert block [%s]", item.Val)
		}
	}

	return nil, it.Errorf("Invalid upsert block")
}

// parseMutationBlock parses the mutation block
func parseMutationBlock(it *lex.ItemIterator) (*pb.Mutation, error) {
	var mu pb.Mutation

	item := it.Item()
	if item.Typ != itemLeftCurl {
		return nil, it.Errorf("Expected { at the start of block. Got: [%s]", item.Val)
	}

	for it.Next() {
		item := it.Item()
		if item.Typ == itemText {
			continue
		}
		if item.Typ == itemRightCurl {
			return &mu, nil
		}
		if item.Typ == itemMutationOp {
			if err := parseMutationOp(it, item.Val, &mu); err != nil {
				return nil, err
			}
		}
	}
	return nil, it.Errorf("Invalid mutation.")
}

// parseMutationOp parses and stores set or delete operation string in Mutation.
func parseMutationOp(it *lex.ItemIterator, op string, mu *pb.Mutation) error {
	parse := false
	for it.Next() {
		item := it.Item()
		if item.Typ == itemText {
			continue
		}
		if item.Typ == itemLeftCurl {
			if parse {
				return it.Errorf("Too many left curls in set mutation.")
			}
			parse = true
		}
		if item.Typ == itemMutationOpContent {
			if !parse {
				return it.Errorf("Mutation syntax invalid.")
			}

			switch op {
			case "set":
				mu.SetJson = []byte(item.Val)
			case "delete":
				mu.DeleteJson = []byte(item.Val)
			case "schema":
				return it.Errorf("Altering schema not supported through http client.")
			case "dropall":
				return it.Errorf("Dropall not supported through http client.")
			default:
				return it.Errorf("Invalid mutation operation.")
			}
		}
		if item.Typ == itemRightCurl {
			return nil
		}
	}
	return it.Errorf("Invalid mutation formatting.")
}
