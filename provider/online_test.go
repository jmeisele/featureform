//go:build online
// +build online

// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at https://mozilla.org/MPL/2.0/.
package provider

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"os"
	"reflect"
	"testing"

	"github.com/featureform/helpers"

	"github.com/alicebob/miniredis"
	pc "github.com/featureform/provider/provider_config"
	pt "github.com/featureform/provider/provider_type"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
)

func mockRedis() *miniredis.Miniredis {
	s, err := miniredis.Run()
	if err != nil {
		panic(err)
	}
	return s
}

type OnlineResource struct {
	Entity string
	Value  interface{}
	Type   ValueType
}

var provider = flag.String("provider", "all", "provider to perform test on")

func TestOnlineStores(t *testing.T) {
	err := godotenv.Load("../.env")
	if err != nil {
		fmt.Println(err)
	}

	testFns := map[string]func(*testing.T, OnlineStore){
		"CreateGetTable":     testCreateGetTable,
		"TableAlreadyExists": testTableAlreadyExists,
		"TableNotFound":      testTableNotFound,
		"SetGetEntity":       testSetGetEntity,
		"EntityNotFound":     testEntityNotFound,
		"MassTableWrite":     testMassTableWrite,
		"TypeCasting":        testTypeCasting,
	}

	// Redis (Mock)
	redisMockInit := func(mRedis *miniredis.Miniredis) pc.RedisConfig {
		mockRedisAddr := mRedis.Addr()
		redisMockConfig := &pc.RedisConfig{
			Addr: mockRedisAddr,
		}
		return *redisMockConfig
	}

	//Redis (Live)
	redisInsecureInit := func() pc.RedisConfig {
		redisInsecurePort := os.Getenv("REDIS_INSECURE_PORT")
		insecureAddr := fmt.Sprintf("%s:%s", "localhost", redisInsecurePort)
		redisInsecureConfig := &pc.RedisConfig{
			Addr: insecureAddr,
		}
		return *redisInsecureConfig
	}

	redisSecureInit := func() pc.RedisConfig {
		redisSecurePort := os.Getenv("REDIS_SECURE_PORT")
		redisPassword := os.Getenv("REDIS_PASSWORD")
		secureAddr := fmt.Sprintf("%s:%s", "localhost", redisSecurePort)
		redisSecureConfig := &pc.RedisConfig{
			Addr:     secureAddr,
			Password: redisPassword,
		}
		return *redisSecureConfig
	}

	//Cassandra
	cassandraInit := func() pc.CassandraConfig {
		cassandraAddr := "localhost:9042"
		cassandraUsername := os.Getenv("CASSANDRA_USER")
		cassandraPassword := os.Getenv("CASSANDRA_PASSWORD")
		cassandraConfig := &pc.CassandraConfig{
			Addr:        cassandraAddr,
			Username:    cassandraUsername,
			Consistency: "ONE",
			Password:    cassandraPassword,
			Replication: 3,
		}
		return *cassandraConfig
	}

	//Firestore
	firestoreInit := func() pc.FirestoreConfig {
		projectID := os.Getenv("FIRESTORE_PROJECT")
		firestoreCredentials := os.Getenv("FIRESTORE_CRED")
		JSONCredentials, err := ioutil.ReadFile(firestoreCredentials)
		if err != nil {
			panic(fmt.Sprintf("Could not open firestore credentials: %v", err))
		}

		var credentialsDict map[string]interface{}
		err = json.Unmarshal(JSONCredentials, &credentialsDict)
		if err != nil {
			panic(fmt.Errorf("cannot unmarshal big query credentials: %v", err))
		}

		firestoreConfig := &pc.FirestoreConfig{
			Collection:  "featureform_test",
			ProjectID:   projectID,
			Credentials: credentialsDict,
		}
		return *firestoreConfig
	}

	dynamoInit := func() pc.DynamodbConfig {
		dynamoAccessKey := os.Getenv("DYNAMO_ACCESS_KEY")
		dynamoSecretKey := os.Getenv("DYNAMO_SECRET_KEY")
		dynamoConfig := &pc.DynamodbConfig{
			Region:    "us-east-1",
			AccessKey: dynamoAccessKey,
			SecretKey: dynamoSecretKey,
		}
		return *dynamoConfig
	}

	blobAzureInit := func() pc.OnlineBlobConfig {
		azureConfig := pc.AzureFileStoreConfig{
			AccountName:   helpers.GetEnv("AZURE_ACCOUNT_NAME", ""),
			AccountKey:    helpers.GetEnv("AZURE_ACCOUNT_KEY", ""),
			ContainerName: helpers.GetEnv("AZURE_CONTAINER_NAME", "newcontainer"),
			Path:          "featureform/onlinetesting",
		}
		blobConfig := &pc.OnlineBlobConfig{
			Type:   pc.Azure,
			Config: azureConfig,
		}
		return *blobConfig
	}

	mongoDBInit := func() pc.MongoDBConfig {
		mongoConfig := &pc.MongoDBConfig{
			Host:       helpers.GetEnv("MONGODB_HOST", ""),
			Port:       helpers.GetEnv("MONGODB_PORT", ""),
			Username:   helpers.GetEnv("MONGODB_USERNAME", ""),
			Password:   helpers.GetEnv("MONGODB_PASSWORD", ""),
			Database:   helpers.GetEnv("MONGODB_DATABASE", ""),
			Throughput: 1000,
		}
		return *mongoConfig
	}

	type testMember struct {
		t               pt.Type
		subType         string
		c               pc.SerializedConfig
		integrationTest bool
	}

	testList := []testMember{}

	if *provider == "memory" || *provider == "" {
		testList = append(testList, testMember{pt.LocalOnline, "", []byte{}, false})
	}
	if *provider == "redis_mock" || *provider == "" {
		miniRedis := mockRedis()
		defer miniRedis.Close()
		testList = append(testList, testMember{pt.RedisOnline, "_MOCK", redisMockInit(miniRedis).Serialized(), false})
	}
	if *provider == "redis_insecure" || *provider == "" {
		testList = append(testList, testMember{pt.RedisOnline, "_INSECURE", redisInsecureInit().Serialized(), true})
	}
	if *provider == "redis_secure" || *provider == "" {
		testList = append(testList, testMember{pt.RedisOnline, "_SECURE", redisSecureInit().Serialized(), true})
	}
	if *provider == "cassandra" || *provider == "" {
		testList = append(testList, testMember{pt.CassandraOnline, "", cassandraInit().Serialized(), true})
	}
	if *provider == "firestore" || *provider == "" {
		testList = append(testList, testMember{pt.FirestoreOnline, "", firestoreInit().Serialize(), true})
	}
	if *provider == "dynamo" || *provider == "" {
		testList = append(testList, testMember{pt.DynamoDBOnline, "", dynamoInit().Serialized(), true})
	}
	if *provider == "azure_blob" || *provider == "" {
		testList = append(testList, testMember{pt.BlobOnline, "_AZURE", blobAzureInit().Serialized(), true})
	}
	if *provider == "mongodb" || *provider == "" {
		testList = append(testList, testMember{pt.MongoDBOnline, "", mongoDBInit().Serialized(), true})
	}

	for _, testItem := range testList {
		if testing.Short() && testItem.integrationTest {
			t.Logf("Skipping %s, because it is an integration test", testItem.t)
			continue
		}
		for name, fn := range testFns {
			provider, err := Get(testItem.t, testItem.c)
			if err != nil {
				t.Fatalf("Failed to get provider %s: %s", testItem.t, err)
			}
			store, err := provider.AsOnlineStore()
			if err != nil {
				t.Fatalf("Failed to use provider %s as OnlineStore: %s", testItem.t, err)
			}
			var prefix string
			if testItem.integrationTest {
				prefix = "INTEGRATION"
			} else {
				prefix = "UNIT"
			}
			testName := fmt.Sprintf("%s%s_%s_%s", testItem.t, testItem.subType, prefix, name)
			t.Run(testName, func(t *testing.T) {
				fn(t, store)
			})
			if err := store.Close(); err != nil {
				t.Fatalf("Failed to close online store %s: %v", testItem.t, err)
			}
		}
		// t.Run("TestConsistency", func(t *testing.T) {
		// 	testConsistency(t, testItem.t, testItem.c)
		// })
	}
}

func randomFeatureVariant() (string, string) {
	return uuid.NewString(), uuid.NewString()
}

func testCreateGetTable(t *testing.T, store OnlineStore) {
	mockFeature, mockVariant := randomFeatureVariant()
	defer store.DeleteTable(mockFeature, mockVariant)
	if tab, err := store.CreateTable(mockFeature, mockVariant, String); tab == nil || err != nil {
		t.Fatalf("Failed to create table: %s", err)
	}
	if tab, err := store.GetTable(mockFeature, mockVariant); tab == nil || err != nil {
		t.Fatalf("Failed to get table: %s", err)
	}
}

func testTableAlreadyExists(t *testing.T, store OnlineStore) {
	mockFeature, mockVariant := randomFeatureVariant()
	defer store.DeleteTable(mockFeature, mockVariant)
	if _, err := store.CreateTable(mockFeature, mockVariant, String); err != nil {
		t.Fatalf("Failed to create table: %s", err)
	}
	if _, err := store.CreateTable(mockFeature, mockVariant, String); err == nil {
		t.Fatalf("Succeeded in creating table twice")
	} else if casted, valid := err.(*TableAlreadyExists); !valid {
		t.Fatalf("Wrong error for table already exists: %T", err)
	} else if casted.Error() == "" {
		t.Fatalf("TableAlreadyExists has empty error message")
	}
}

func testTableNotFound(t *testing.T, store OnlineStore) {
	mockFeature, mockVariant := randomFeatureVariant()
	if _, err := store.GetTable(mockFeature, mockVariant); err == nil {
		t.Fatalf("Succeeded in getting non-existent table")
	} else if casted, valid := err.(*TableNotFound); !valid {
		t.Fatalf("Wrong error for table not found: %s,%T", err, err)
	} else if casted.Error() == "" {
		t.Fatalf("TableNotFound has empty error message")
	}
}

func testSetGetEntity(t *testing.T, store OnlineStore) {
	mockFeature, mockVariant := randomFeatureVariant()
	defer store.DeleteTable(mockFeature, mockVariant)
	entity, val := "e", "val"
	defer store.DeleteTable(mockFeature, mockVariant)
	tab, err := store.CreateTable(mockFeature, mockVariant, String)
	if err != nil {
		t.Fatalf("Failed to create table: %s", err)
	}
	if err := tab.Set(entity, val); err != nil {
		t.Fatalf("Failed to set entity: %s", err)
	}
	gotVal, err := tab.Get(entity)
	if err != nil {
		t.Fatalf("Failed to get entity: %s", err)
	}
	if !reflect.DeepEqual(val, gotVal) {
		t.Fatalf("Values are not the same %v %v", val, gotVal)
	}
}

func testEntityNotFound(t *testing.T, store OnlineStore) {
	mockFeature, mockVariant := uuid.NewString(), "v"
	entity := "e"
	defer store.DeleteTable(mockFeature, mockVariant)
	tab, err := store.CreateTable(mockFeature, mockVariant, String)
	if err != nil {
		t.Fatalf("Failed to create table: %s", err)
	}
	if _, err := tab.Get(entity); err == nil {
		t.Fatalf("succeeded in getting non-existent entity")
	} else if casted, valid := err.(*EntityNotFound); !valid {
		t.Fatalf("Wrong error for entity not found: %T", err)
	} else if casted.Error() == "" {
		t.Fatalf("EntityNotFound has empty error message")
	}
}

func testMassTableWrite(t *testing.T, store OnlineStore) {
	tableList := make([]ResourceID, 10)
	for i := range tableList {
		mockFeature, mockVariant := randomFeatureVariant()
		tableList[i] = ResourceID{mockFeature, mockVariant, Feature}
	}
	entityList := make([]string, 10)
	for i := range entityList {
		entityList[i] = uuid.New().String()
	}
	for i := range tableList {
		tab, err := store.CreateTable(tableList[i].Name, tableList[i].Variant, ValueType("int"))
		if err != nil {
			t.Fatalf("could not create table %v in online store: %v", tableList[i], err)
		}
		defer store.DeleteTable(tableList[i].Name, tableList[i].Variant)
		for j := range entityList {
			if err := tab.Set(entityList[j], 1); err != nil {
				t.Fatalf("could not set entity %v in table %v: %v", entityList[j], tableList[i], err)
			}
		}
	}
	for i := range tableList {
		tab, err := store.GetTable(tableList[i].Name, tableList[i].Variant)
		if err != nil {
			t.Fatalf("could not get table %v in online store: %v", tableList[i], err)
		}
		for j := range entityList {
			val, err := tab.Get(entityList[j])
			if err != nil {
				t.Fatalf("could not get entity %v in table %v: %v", entityList[j], tableList[i], err)
			}
			if val != 1 {
				t.Fatalf("could not get correct value from entity list. Wanted %v, got %v", 1, val)
			}
		}
	}
}

func testTypeCasting(t *testing.T, store OnlineStore) {
	onlineResources := []OnlineResource{
		{
			Entity: "a",
			Value:  int(1),
			Type:   Int,
		},
		{
			Entity: "b",
			Value:  int64(1),
			Type:   Int64,
		},
		{
			Entity: "c",
			Value:  float32(1.0),
			Type:   Float32,
		},
		{
			Entity: "d",
			Value:  float64(1.0),
			Type:   Float64,
		},
		{
			Entity: "e",
			Value:  "1.0",
			Type:   String,
		},
		{
			Entity: "f",
			Value:  false,
			Type:   Bool,
		},
	}
	for _, resource := range onlineResources {
		featureName := uuid.New().String()
		tab, err := store.CreateTable(featureName, "", resource.Type)
		if err != nil {
			t.Fatalf("Failed to create table: %s", err)
		}
		if err := tab.Set(resource.Entity, resource.Value); err != nil {
			t.Fatalf("Failed to set entity: %s", err)
		}
		gotVal, err := tab.Get(resource.Entity)
		if err != nil {
			t.Fatalf("Failed to get entity: %s", err)
		}
		if !reflect.DeepEqual(resource.Value, gotVal) {
			t.Fatalf("Values are not the same %v, type %T. %v, type %T", resource.Value, resource.Value, gotVal, gotVal)
		}
		store.DeleteTable(featureName, "")
	}
}

// func testConsistency(t *testing.T, tp Type, config SerializedConfig) {
// 	createConnection := func(t *testing.T, tp Type, config SerializedConfig) OnlineStore {
// 		provider, err := Get(tp, config)
// 		if err != nil {
// 			t.Errorf("Could not get provider: %s", err.Error())
// 		}
// 		store, err := provider.AsOnlineStore()
// 		if err != nil {
// 			t.Errorf("Could not get provider as online store: %s", err.Error())
// 		}
// 		return store
// 	}

// 	for i := 1; i < 10; i++ {
// 		store := createConnection(t, tp, config)
// 		featureName := fmt.Sprintf("feature_%s", uuid.NewString())
// 		_, err := store.CreateTable(featureName, "default", String)
// 		if err != nil {
// 			t.Errorf("could not create table: %s", err.Error())
// 		}
// 		store.Close()
// 		if err != nil {
// 			t.Errorf("could not close store after create: %s", err.Error())
// 		}

// 		store = createConnection(t, tp, config)
// 		_, err = store.GetTable(featureName, "default")
// 		if err != nil {
// 			t.Errorf("could not get initial table: %s", err.Error())
// 		}

// 		err = store.DeleteTable(featureName, "default")
// 		if err != nil {
// 			t.Errorf("could not delete table: %s", err.Error())
// 		}
// 		store.Close()
// 		if err != nil {
// 			t.Errorf("could not close store after delete: %s", err.Error())
// 		}
// 	}
// }

func TestFirestoreConfig_Deserialize(t *testing.T) {
	content, err := ioutil.ReadFile("connection/connection_configs.json")
	if err != nil {
		t.Fatalf(err.Error())
	}
	var payload map[string]interface{}
	err = json.Unmarshal(content, &payload)
	if err != nil {
		t.Fatalf(err.Error())
	}
	testConfig := payload["Firestore"].(map[string]interface{})

	fsconfig := pc.FirestoreConfig{
		ProjectID:   testConfig["ProjectID"].(string),
		Collection:  testConfig["Collection"].(string),
		Credentials: testConfig["Credentials"].(map[string]interface{}),
	}

	serialized := fsconfig.Serialize()

	type fields struct {
		Collection  string
		ProjectID   string
		Credentials map[string]interface{}
	}
	type args struct {
		config pc.SerializedConfig
	}
	tests := []struct {
		name    string
		fields  fields
		args    args
		wantErr bool
	}{
		{
			name: "TestCredentials",
			fields: fields{
				ProjectID:   testConfig["ProjectID"].(string),
				Collection:  testConfig["Collection"].(string),
				Credentials: testConfig["Credentials"].(map[string]interface{}),
			},
			args: args{
				config: serialized,
			},
			wantErr: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &pc.FirestoreConfig{
				Collection:  tt.fields.Collection,
				ProjectID:   tt.fields.ProjectID,
				Credentials: tt.fields.Credentials,
			}
			if err := r.Deserialize(tt.args.config); (err != nil) != tt.wantErr {
				t.Errorf("Deserialize() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
