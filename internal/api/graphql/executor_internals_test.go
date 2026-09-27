package graphql

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vektah/gqlparser/v2/ast"
)

func TestDepthAndComplexityLimiters(t *testing.T) {
	sel := []ast.Selection{
		&ast.Field{
			Name: "node",
			SelectionSet: ast.SelectionSet{
				&ast.Field{Name: "id"},
				&ast.Field{Name: "name"},
			},
		},
	}
	c := complexity(sel)
	assert.Greater(t, c, 0)

	d := depth(sel, 0)
	assert.Greater(t, d, 0)
}

func TestCursorHandling(t *testing.T) {
	c := EncodeCursor("test_id_123", "id", "ASC")
	assert.NotEmpty(t, c)

	decoded, err := DecodeCursor(c)
	require.NoError(t, err)
	assert.Equal(t, "test_id_123", decoded.LastID)

	_, err = DecodeCursor("invalid_cursor_string_tampered")
	require.Error(t, err)
}

func TestResolverErrorSurfacing(t *testing.T) {
	RegisterRoute("Query", "errorTestField", func(r *Resolver, ctx context.Context, args json.RawMessage, vars map[string]any) (any, error) {
		return nil, errors.New("boom resolver error")
	})
	UseResolver(&Resolver{})

	req := &GraphQLRequest{Query: "{ errorTestField }"}
	env, err := executeOperation(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, env)
	require.Len(t, env.Errors, 1)
	assert.Equal(t, "boom resolver error", env.Errors[0].Message)
}

// TestSchemaIntrospectionIsNotSupported pins the current contract: this
// executor resolves a fixed set of Query fields and implements no
// introspection, so __schema is reported as an unknown field rather than
// being answered or silently returning null. A client that needs
// introspection has to be told, and this is the behaviour it will see.
func TestSchemaIntrospectionIsNotSupported(t *testing.T) {
	UseResolver(&Resolver{})
	req := &GraphQLRequest{Query: "{ __schema { types { name } } }"}
	env, err := executeOperation(context.Background(), req)
	require.NoError(t, err)
	require.NotNil(t, env)
	require.Len(t, env.Errors, 1)
	assert.Contains(t, env.Errors[0].Message, `unknown Query field "__schema"`)
	assert.Nil(t, env.Data["__schema"])
}
