// Copyright 2025 Lanka Data Foundation
// SPDX-License-Identifier: Apache-2.0

package mongorepository

import (
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"

	"lk/datafoundation/core-api/db/config"
	"lk/datafoundation/core-api/pkg/graphschema"
)

// newGraphSchemaTestRepo returns a repository that shares the test client from TestMain but
// uses its own database, so these tests never touch a real graph_schema collection. The
// database is dropped before and after each test.
func newGraphSchemaTestRepo(t *testing.T) *MongoRepository {
	t.Helper()
	repo := &MongoRepository{
		client: testRepo.client,
		config: &config.MongoConfig{
			URI:    testRepo.config.URI,
			DBName: testRepo.config.DBName + "_graph_schema_test",
		},
	}

	drop := func() {
		if err := repo.client.Database(repo.config.DBName).Drop(testCtx); err != nil {
			t.Fatalf("failed to drop test database %s: %v", repo.config.DBName, err)
		}
	}
	drop()
	t.Cleanup(drop)
	return repo
}

func loadGraphSchemaFixture(t *testing.T) graphschema.Schema {
	t.Helper()
	src, err := os.ReadFile("../../../pkg/graphschema/testdata/lk_regions_government.json")
	if err != nil {
		t.Fatalf("failed to read graph schema fixture: %v", err)
	}
	schema, err := graphschema.Parse(src)
	if err != nil {
		t.Fatalf("failed to parse graph schema fixture: %v", err)
	}
	return *schema
}

func TestReadLatestGraphSchemaWhenNoneStored(t *testing.T) {
	repo := newGraphSchemaTestRepo(t)

	version, err := repo.ReadLatestGraphSchema(testCtx)
	assert.Nil(t, version)
	assert.True(t, errors.Is(err, ErrGraphSchemaNotFound), "got %v", err)
}

func TestInsertAndReadLatestGraphSchema(t *testing.T) {
	repo := newGraphSchemaTestRepo(t)
	schema := loadGraphSchemaFixture(t)

	before := time.Now().UTC()
	err := repo.InsertGraphSchema(testCtx, &GraphSchemaVersion{
		Version: 1,
		Change:  GraphSchemaChangeReplace,
		Source:  `{"kindDefs": "..."}`,
		Schema:  schema,
	})
	if !assert.NoError(t, err) {
		return
	}

	latest, err := repo.ReadLatestGraphSchema(testCtx)
	if !assert.NoError(t, err) {
		return
	}
	assert.Equal(t, 1, latest.Version)
	assert.Equal(t, GraphSchemaChangeReplace, latest.Change)
	assert.Equal(t, `{"kindDefs": "..."}`, latest.Source)
	// The whole schema survives the round trip, including relationships with several pairs.
	assert.Equal(t, schema, latest.Schema)
	assert.Len(t, latest.Schema.AllowedPairs("located_in"), 2)
	// CreatedAt is filled in; MongoDB stores dates to the millisecond.
	assert.WithinDuration(t, before, latest.CreatedAt, 5*time.Second)
}

func TestInsertGraphSchemaKeepsGivenCreatedAt(t *testing.T) {
	repo := newGraphSchemaTestRepo(t)
	createdAt := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	err := repo.InsertGraphSchema(testCtx, &GraphSchemaVersion{
		Version:   1,
		Change:    GraphSchemaChangeReplace,
		Schema:    loadGraphSchemaFixture(t),
		CreatedAt: createdAt,
	})
	if !assert.NoError(t, err) {
		return
	}

	latest, err := repo.ReadLatestGraphSchema(testCtx)
	if assert.NoError(t, err) {
		assert.True(t, createdAt.Equal(latest.CreatedAt), "got %v", latest.CreatedAt)
	}
}

func TestReadLatestGraphSchemaReturnsHighestVersionAndKeepsHistory(t *testing.T) {
	repo := newGraphSchemaTestRepo(t)
	first := loadGraphSchemaFixture(t)
	second := loadGraphSchemaFixture(t)
	second.Description = "version two"

	assert.NoError(t, repo.InsertGraphSchema(testCtx, &GraphSchemaVersion{
		Version: 1, Change: GraphSchemaChangeReplace, Schema: first,
	}))
	assert.NoError(t, repo.InsertGraphSchema(testCtx, &GraphSchemaVersion{
		Version: 2, Change: GraphSchemaChangePatch, Source: `{"baseVersion": 1}`, Schema: second,
	}))

	latest, err := repo.ReadLatestGraphSchema(testCtx)
	if assert.NoError(t, err) {
		assert.Equal(t, 2, latest.Version)
		assert.Equal(t, GraphSchemaChangePatch, latest.Change)
		assert.Equal(t, "version two", latest.Schema.Description)
	}

	// Older versions are kept.
	count, err := repo.graphSchemaCollection().CountDocuments(testCtx, bson.D{})
	if assert.NoError(t, err) {
		assert.Equal(t, int64(2), count)
	}
	var oldest GraphSchemaVersion
	if assert.NoError(t, repo.graphSchemaCollection().FindOne(testCtx, bson.M{"_id": 1}).Decode(&oldest)) {
		assert.Equal(t, first, oldest.Schema)
	}
}

func TestInsertGraphSchemaRejectsExistingVersion(t *testing.T) {
	repo := newGraphSchemaTestRepo(t)
	original := loadGraphSchemaFixture(t)

	assert.NoError(t, repo.InsertGraphSchema(testCtx, &GraphSchemaVersion{
		Version: 1, Change: GraphSchemaChangeReplace, Schema: original,
	}))

	changed := loadGraphSchemaFixture(t)
	changed.Description = "should not be stored"
	err := repo.InsertGraphSchema(testCtx, &GraphSchemaVersion{
		Version: 1, Change: GraphSchemaChangeReplace, Schema: changed,
	})
	assert.True(t, errors.Is(err, ErrGraphSchemaVersionExists), "got %v", err)

	latest, err := repo.ReadLatestGraphSchema(testCtx)
	if assert.NoError(t, err) {
		assert.Equal(t, original, latest.Schema)
	}
}

// Two writers saving the same version at the same time: exactly one must succeed.
func TestInsertGraphSchemaConcurrentWritersOfSameVersion(t *testing.T) {
	repo := newGraphSchemaTestRepo(t)
	schema := loadGraphSchemaFixture(t)

	const writers = 5
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = repo.InsertGraphSchema(testCtx, &GraphSchemaVersion{
				Version: 1, Change: GraphSchemaChangeReplace, Schema: schema,
			})
		}(i)
	}
	wg.Wait()

	succeeded := 0
	for _, err := range errs {
		if err == nil {
			succeeded++
		} else {
			assert.True(t, errors.Is(err, ErrGraphSchemaVersionExists), "got %v", err)
		}
	}
	assert.Equal(t, 1, succeeded)
}

func TestInsertGraphSchemaRejectsInvalidInput(t *testing.T) {
	repo := newGraphSchemaTestRepo(t)
	schema := loadGraphSchemaFixture(t)

	tests := map[string]struct {
		version       *GraphSchemaVersion
		errorContains string
	}{
		"nil version": {
			version:       nil,
			errorContains: "graph schema version is required",
		},
		"version zero": {
			version:       &GraphSchemaVersion{Version: 0, Change: GraphSchemaChangeReplace, Schema: schema},
			errorContains: "versions start at 1",
		},
		"negative version": {
			version:       &GraphSchemaVersion{Version: -1, Change: GraphSchemaChangeReplace, Schema: schema},
			errorContains: "versions start at 1",
		},
		"unknown change type": {
			version:       &GraphSchemaVersion{Version: 1, Change: "edit", Schema: schema},
			errorContains: `invalid graph schema change "edit"`,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			err := repo.InsertGraphSchema(testCtx, tc.version)
			if assert.Error(t, err) {
				assert.Contains(t, err.Error(), tc.errorContains)
			}
		})
	}

	// Nothing was stored.
	count, err := repo.graphSchemaCollection().CountDocuments(testCtx, bson.D{})
	if assert.NoError(t, err) {
		assert.Equal(t, int64(0), count)
	}
}

// The schema is stored as nested fields, not as an opaque Any or string, so it is readable and
// queryable in MongoDB.
func TestGraphSchemaIsStoredAsNestedFields(t *testing.T) {
	repo := newGraphSchemaTestRepo(t)
	assert.NoError(t, repo.InsertGraphSchema(testCtx, &GraphSchemaVersion{
		Version: 1, Change: GraphSchemaChangeReplace, Schema: loadGraphSchemaFixture(t),
	}))

	// Query inside the stored schema.
	count, err := repo.graphSchemaCollection().CountDocuments(testCtx, bson.M{
		"schema.relationshipDefs.name": "located_in",
		"schema.kindDefs.major":        "Organization",
	})
	if assert.NoError(t, err) {
		assert.Equal(t, int64(1), count)
	}

	var raw bson.M
	if assert.NoError(t, repo.graphSchemaCollection().FindOne(testCtx, bson.M{"_id": 1}).Decode(&raw)) {
		stored, ok := raw["schema"].(bson.M)
		if assert.True(t, ok, "schema should be an embedded document, got %T", raw["schema"]) {
			assert.IsType(t, bson.A{}, stored["kindDefs"])
			assert.IsType(t, bson.A{}, stored["relationshipDefs"])
		}
		// createdAt is a real BSON date, not a string.
		assert.IsType(t, primitive.DateTime(0), raw["createdAt"])
	}
}
