// Copyright 2025 Lanka Data Foundation
// SPDX-License-Identifier: Apache-2.0

package mongorepository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"lk/datafoundation/core-api/pkg/graphschema"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// graphSchemaCollection holds one document per graph schema version, in the same database as
// the entity metadata.
const graphSchemaCollection = "graph_schema"

// How a graph schema version was created.
const (
	GraphSchemaChangeReplace = "replace" // the whole schema was submitted (PUT)
	GraphSchemaChangePatch   = "patch"   // a patch was applied to the previous version (PATCH)
)

var (
	// ErrGraphSchemaNotFound is returned when no graph schema has been stored yet.
	ErrGraphSchemaNotFound = errors.New("graph schema not found")
	// ErrGraphSchemaVersionExists is returned when a version is already stored, which means
	// another writer saved the same version first.
	ErrGraphSchemaVersionExists = errors.New("graph schema version already exists")
)

// GraphSchemaVersion is one stored version of the graph schema. Every version holds the
// complete schema, so reading a version never replays patches. Versions are only ever inserted,
// never updated or deleted.
type GraphSchemaVersion struct {
	// Version is the document's _id. Versions start at 1 and increase by 1, and the unique _id
	// makes concurrent writes of the same version fail.
	Version int `bson:"_id"`
	// Change is GraphSchemaChangeReplace or GraphSchemaChangePatch.
	Change string `bson:"change"`
	// Source is what the client sent: the full schema document, or the patch body.
	Source string `bson:"source"`
	// Schema is the complete schema as of this version, stored as nested fields.
	Schema    graphschema.Schema `bson:"schema"`
	CreatedAt time.Time          `bson:"createdAt"`
}

func (repo *MongoRepository) graphSchemaCollection() *mongo.Collection {
	return repo.client.Database(repo.config.DBName).Collection(graphSchemaCollection)
}

// InsertGraphSchema stores a new graph schema version. CreatedAt is set to the current time if
// it is zero. It returns ErrGraphSchemaVersionExists if the version is already stored.
func (repo *MongoRepository) InsertGraphSchema(ctx context.Context, version *GraphSchemaVersion) error {
	if version == nil {
		return fmt.Errorf("graph schema version is required")
	}
	if version.Version < 1 {
		return fmt.Errorf("invalid graph schema version %d: versions start at 1", version.Version)
	}
	if version.Change != GraphSchemaChangeReplace && version.Change != GraphSchemaChangePatch {
		return fmt.Errorf("invalid graph schema change %q: expected %q or %q",
			version.Change, GraphSchemaChangeReplace, GraphSchemaChangePatch)
	}
	if version.CreatedAt.IsZero() {
		version.CreatedAt = time.Now().UTC()
	}

	if _, err := repo.graphSchemaCollection().InsertOne(ctx, version); err != nil {
		if mongo.IsDuplicateKeyError(err) {
			return fmt.Errorf("%w: version %d", ErrGraphSchemaVersionExists, version.Version)
		}
		return fmt.Errorf("error inserting graph schema version %d: %w", version.Version, err)
	}
	return nil
}

// ReadLatestGraphSchema returns the graph schema version with the highest version number, or
// ErrGraphSchemaNotFound if none is stored.
func (repo *MongoRepository) ReadLatestGraphSchema(ctx context.Context) (*GraphSchemaVersion, error) {
	findOptions := options.FindOne().SetSort(bson.D{{Key: "_id", Value: -1}})

	var version GraphSchemaVersion
	err := repo.graphSchemaCollection().FindOne(ctx, bson.D{}, findOptions).Decode(&version)
	if err != nil {
		if errors.Is(err, mongo.ErrNoDocuments) {
			return nil, ErrGraphSchemaNotFound
		}
		return nil, fmt.Errorf("error reading latest graph schema: %w", err)
	}
	return &version, nil
}
