package provider

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/credentials"

	"github.com/featureform/helpers"
	"github.com/featureform/logging"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/emr"
	databricks "github.com/databricks/databricks-sdk-go"
	"github.com/databricks/databricks-sdk-go/service/jobs"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"golang.org/x/exp/slices"

	emrTypes "github.com/aws/aws-sdk-go-v2/service/emr/types"
	pc "github.com/featureform/provider/provider_config"
)

type JobType string

const (
	Materialize       JobType = "Materialization"
	Transform         JobType = "Transformation"
	CreateTrainingSet JobType = "Training Set"
)

const MATERIALIZATION_ID_SEGMENTS = 3
const ENTITY_INDEX = 0
const VALUE_INDEX = 1
const TIMESTAMP_INDEX = 2

type DatabricksResultState string

const (
	Success   DatabricksResultState = "SUCCESS"
	Failed    DatabricksResultState = "FAILED"
	Timeout   DatabricksResultState = "TIMEOUT"
	Cancelled DatabricksResultState = "CANCELLED"
)

type DatabricksExecutor struct {
	client  *databricks.WorkspaceClient
	cluster string
	config  pc.DatabricksConfig
}

func (e *EMRExecutor) PythonFileURI(store FileStore) string {
	return ""
}

func (db *DatabricksExecutor) PythonFileURI(store FileStore) string {
	filePath := helpers.GetEnv("SPARK_SCRIPT_PATH", "/scripts/spark/offline_store_spark_runner.py")
	return store.PathWithPrefix(filePath[1:], true)
}

func readAndUploadFile(filePath string, storePath string, store FileStore) error {
	fileExists, _ := store.Exists(storePath)
	if fileExists {
		return nil
	}

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("could not open file: %v", err)
	}

	fileStats, err := f.Stat()
	if err != nil {
		return fmt.Errorf("could not get file stats: %v", err)
	}

	pythonScriptBytes := make([]byte, fileStats.Size())
	_, err = f.Read(pythonScriptBytes)
	if err != nil {
		return fmt.Errorf("could not read python script because %v", err)
	}
	if err := store.Write(storePath, pythonScriptBytes); err != nil {
		return fmt.Errorf("could not write to python script: %v", err)
	}
	return nil
}

func (db *DatabricksExecutor) InitializeExecutor(store FileStore) error {
	sparkScriptPath := helpers.GetEnv("SPARK_SCRIPT_PATH", "/scripts/spark/offline_store_spark_runner.py")[1:]
	pythonInitScriptPath := helpers.GetEnv("PYTHON_INIT_PATH", "/scripts/spark/python_packages.sh")[1:]

	err := readAndUploadFile(sparkScriptPath, store.PathWithPrefix(sparkScriptPath, false), store)
	sparkExists, _ := store.Exists(store.PathWithPrefix(sparkScriptPath, false))
	if err != nil && !sparkExists {
		return fmt.Errorf("could not upload spark script: Path: %s, Error: %v", store.PathWithPrefix(sparkScriptPath, false), err)
	}

	err = readAndUploadFile(pythonInitScriptPath, store.PathWithPrefix(pythonInitScriptPath, false), store)
	initExists, _ := store.Exists(store.PathWithPrefix(pythonInitScriptPath, false))
	if err != nil && !initExists {
		return fmt.Errorf("could not upload python initialization script: Path: %s, Error: %v", store.PathWithPrefix(pythonInitScriptPath, false), err)
	}
	return nil
}

func NewDatabricksExecutor(databricksConfig pc.DatabricksConfig) (SparkExecutor, error) {
	client := databricks.Must(
		databricks.NewWorkspaceClient(&databricks.Config{
			Host:     databricksConfig.Host,
			Token:    databricksConfig.Token,
			Username: databricksConfig.Username,
			Password: databricksConfig.Password,
		}))
	return &DatabricksExecutor{
		client:  client,
		cluster: databricksConfig.Cluster,
		config:  databricksConfig,
	}, nil
}

func (db *DatabricksExecutor) RunSparkJob(args []string, store FileStore) error {
	//set spark configuration
	// clusterClient := db.client.Clusters()
	// setConfigReq := clusterHTTPModels.EditReq{
	// 	ClusterID: db.cluster,
	// 	SparkConf: clusterModels.SparkConfPair{
	// 		Key:   "fs.azure.account.key.testingstoragegen.dfs.core.windows.net", //change to one based on account name
	// 		Value: helpers.GetEnv("AZURE_ACCOUNT_KEY", ""),
	// 	},
	// }
	//TODO: resolve error: "Custom containers is turned off for your deployment. Please contact your workspace administrator to use this feature."
	// need to specify spark version
	// if err := clusterClient.Edit(setConfigReq); err != nil {
	// 	return fmt.Errorf("Could not modify cluster to accept spark configs; %v", err)
	// }
	pythonTask := jobs.SparkPythonTask{
		PythonFile: db.PythonFileURI(store),
		Parameters: args,
	}
	ctx := context.Background()
	id := uuid.New().String()

	jobToRun, err := db.client.Jobs.Create(ctx, jobs.CreateJob{
		Name: fmt.Sprintf("featureform-job-%s", id),
		Tasks: []jobs.JobTaskSettings{
			{
				TaskKey:           fmt.Sprintf("featureform-task-%s", id),
				ExistingClusterId: db.cluster,
				SparkPythonTask:   &pythonTask,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("error creating job: %v", err)
	}

	_, err = db.client.Jobs.RunNowAndWait(ctx, jobs.RunNow{
		JobId: jobToRun.JobId,
	})
	if err != nil {
		return fmt.Errorf("the '%v' job failed: %v", jobToRun.JobId, err)
	}

	return nil
}

type PythonOfflineQueries interface {
	materializationCreate(schema ResourceSchema) string
	trainingSetCreate(def TrainingSetDef, featureSchemas []ResourceSchema, labelSchema ResourceSchema) string
}

type defaultPythonOfflineQueries struct{}

func (q defaultPythonOfflineQueries) materializationCreate(schema ResourceSchema) string {
	timestampColumn := schema.TS
	// without timestamp, assumes each entity only has single entry
	if schema.TS == "" {
		return fmt.Sprintf("SELECT %s AS entity, %s AS value, 0 as ts, ROW_NUMBER() over (ORDER BY (SELECT NULL)) AS row_number FROM source_0", schema.Entity, schema.Value)
	}
	return fmt.Sprintf(
		"SELECT entity, value, ts, ROW_NUMBER() over (ORDER BY (SELECT NULL)) AS row_number FROM "+
			"(SELECT entity, value, ts, rn FROM (SELECT %s AS entity, %s AS value, %s AS ts, "+
			"ROW_NUMBER() OVER (PARTITION BY %s ORDER BY %s DESC) AS rn FROM %s) t WHERE rn=1) t2",
		schema.Entity, schema.Value, timestampColumn, schema.Entity, timestampColumn, "source_0")
}

func featureColumnName(id ResourceID) string {
	return fmt.Sprintf("%s__%s__%s", id.Type, id.Name, id.Variant)
}

func (q defaultPythonOfflineQueries) trainingSetCreate(def TrainingSetDef, featureSchemas []ResourceSchema, labelSchema ResourceSchema) string {
	columns := make([]string, 0)
	joinQueries := make([]string, 0)
	feature_timestamps := make([]string, 0)
	for i, feature := range def.Features {
		featureColumnName := featureColumnName(feature)
		columns = append(columns, featureColumnName)
		var featureWindowQuery string
		// if no timestamp column, set to default generated by resource registration
		if featureSchemas[i].TS == "" {
			featureWindowQuery = fmt.Sprintf("SELECT * FROM (SELECT %s as t%d_entity, %s as %s, 0 as t%d_ts FROM source_%d) ORDER BY t%d_ts ASC", featureSchemas[i].Entity, i+1, featureSchemas[i].Value, featureColumnName, i+1, i+1, i+1)
		} else {
			featureWindowQuery = fmt.Sprintf("SELECT * FROM (SELECT %s as t%d_entity, %s as %s, %s as t%d_ts FROM source_%d) ORDER BY t%d_ts ASC", featureSchemas[i].Entity, i+1, featureSchemas[i].Value, featureColumnName, featureSchemas[i].TS, i+1, i+1, i+1)
		}
		featureJoinQuery := fmt.Sprintf("LEFT OUTER JOIN (%s) t%d ON (t%d_entity = entity AND t%d_ts <= label_ts)", featureWindowQuery, i+1, i+1, i+1)
		joinQueries = append(joinQueries, featureJoinQuery)
		feature_timestamps = append(feature_timestamps, fmt.Sprintf("t%d_ts", i+1))
	}
	for i, lagFeature := range def.LagFeatures {
		lagFeaturesOffset := len(def.Features)
		idx := slices.IndexFunc(def.Features, func(id ResourceID) bool {
			return id.Name == lagFeature.FeatureName && id.Variant == lagFeature.FeatureVariant
		})
		lagSource := fmt.Sprintf("source_%d", idx)
		lagColumnName := sanitize(lagFeature.LagName)
		if lagFeature.LagName == "" {
			lagColumnName = sanitize(fmt.Sprintf("%s_%s_lag_%s", lagFeature.FeatureName, lagFeature.FeatureVariant, lagFeature.LagDelta))
		}
		columns = append(columns, lagColumnName)
		timeDeltaSeconds := lagFeature.LagDelta.Seconds() //parquet stores time as microseconds
		curIdx := lagFeaturesOffset + i + 1
		var lagWindowQuery string
		if featureSchemas[idx].TS == "" {
			lagWindowQuery = fmt.Sprintf("SELECT * FROM (SELECT %s as t%d_entity, %s as %s, 0 as t%d_ts FROM %s) ORDER BY t%d_ts ASC", featureSchemas[idx].Entity, curIdx, featureSchemas[idx].Value, lagColumnName, curIdx, lagSource, curIdx)
		} else {
			lagWindowQuery = fmt.Sprintf("SELECT * FROM (SELECT %s as t%d_entity, %s as %s, %s as t%d_ts FROM %s) ORDER BY t%d_ts ASC", featureSchemas[idx].Entity, curIdx, featureSchemas[idx].Value, lagColumnName, featureSchemas[idx].TS, curIdx, lagSource, curIdx)
		}
		lagJoinQuery := fmt.Sprintf("LEFT OUTER JOIN (%s) t%d ON (t%d_entity = entity AND DATETIME(t%d_ts, '+%f seconds') <= label_ts)", lagWindowQuery, curIdx, curIdx, curIdx, timeDeltaSeconds)
		joinQueries = append(joinQueries, lagJoinQuery)
		feature_timestamps = append(feature_timestamps, fmt.Sprintf("t%d_ts", curIdx))
	}
	columnStr := strings.Join(columns, ", ")
	joinQueryString := strings.Join(joinQueries, " ")
	var labelWindowQuery string
	if labelSchema.TS == "" {
		labelWindowQuery = fmt.Sprintf("SELECT %s AS entity, %s AS value, 0 AS label_ts FROM source_0", labelSchema.Entity, labelSchema.Value)
	} else {
		labelWindowQuery = fmt.Sprintf("SELECT %s AS entity, %s AS value, %s AS label_ts FROM source_0", labelSchema.Entity, labelSchema.Value, labelSchema.TS)
	}
	labelPartitionQuery := fmt.Sprintf("(SELECT * FROM (SELECT entity, value, label_ts FROM (%s) t ) t0)", labelWindowQuery)
	labelJoinQuery := fmt.Sprintf("%s %s", labelPartitionQuery, joinQueryString)

	timeStamps := strings.Join(feature_timestamps, ", ")
	timeStampsDesc := strings.Join(feature_timestamps, " DESC,")
	fullQuery := fmt.Sprintf("SELECT %s, value AS %s, entity, label_ts, %s, ROW_NUMBER() over (PARTITION BY entity, value, label_ts ORDER BY label_ts DESC, %s DESC) as row_number FROM (%s) tt", columnStr, featureColumnName(def.Label), timeStamps, timeStampsDesc, labelJoinQuery)
	finalQuery := fmt.Sprintf("SELECT %s, %s FROM (SELECT * FROM (SELECT *, row_number FROM (%s) WHERE row_number=1 ))  ORDER BY label_ts", columnStr, featureColumnName(def.Label), fullQuery)
	return finalQuery
}

type SparkOfflineStore struct {
	Executor SparkExecutor
	Store    FileStore
	Logger   *zap.SugaredLogger
	query    *defaultPythonOfflineQueries
	BaseProvider
}

func (store *SparkOfflineStore) AsOfflineStore() (OfflineStore, error) {
	return store, nil
}

func (store *SparkOfflineStore) Close() error {
	return nil
}

func sparkOfflineStoreFactory(config pc.SerializedConfig) (Provider, error) {
	sc := pc.SparkConfig{}
	logger := logging.NewLogger("spark")
	if err := sc.Deserialize(config); err != nil {
		logger.Errorw("Invalid config to initialize spark offline store", err)
		return nil, fmt.Errorf("invalid spark config: %v", config)
	}
	logger.Infow("Creating Spark executor:", "type", sc.ExecutorType)
	exec, err := NewSparkExecutor(sc.ExecutorType, sc.ExecutorConfig, logger)
	if err != nil {
		logger.Errorw("Failure initializing Spark executor", "type", sc.ExecutorType, "error", err)
		return nil, err
	}

	logger.Infow("Creating Spark store:", "type", sc.StoreType)
	serializedFilestoreConfig, err := sc.StoreConfig.Serialize()
	if err != nil {
		return nil, fmt.Errorf("could not serialize databricks Config, %v", err)
	}
	store, err := CreateFileStore(string(sc.StoreType), Config(serializedFilestoreConfig))
	if err != nil {
		logger.Errorw("Failure initializing blob store", "type", sc.StoreType, "error", err)
		return nil, err
	}
	logger.Info("Uploading Spark script to store")

	logger.Debugf("Store type: %s", sc.StoreType)
	if err := exec.InitializeExecutor(store); err != nil {
		logger.Errorw("Failure initializing executor", "error", err)
		return nil, err
	}
	logger.Info("Created Spark Offline Store")
	queries := defaultPythonOfflineQueries{}
	sparkOfflineStore := SparkOfflineStore{
		Executor: exec,
		Store:    store,
		Logger:   logger,
		query:    &queries,
		BaseProvider: BaseProvider{
			ProviderType:   "SPARK_OFFLINE",
			ProviderConfig: config,
		},
	}
	return &sparkOfflineStore, nil
}

type SparkExecutor interface {
	RunSparkJob(args []string, store FileStore) error
	InitializeExecutor(store FileStore) error
	PythonFileURI(store FileStore) string
	SparkSubmitArgs(destPath string, cleanQuery string, sourceList []string, jobType JobType, store FileStore) []string
	GetDFArgs(outputURI string, code string, sources []string, store FileStore) ([]string, error)
}

type EMRExecutor struct {
	client      *emr.Client
	clusterName string
	logger      *zap.SugaredLogger
}

func (e EMRExecutor) InitializeExecutor(store FileStore) error {
	sparkScriptPath := helpers.GetEnv("SPARK_SCRIPT_PATH", "/scripts/offline_store_spark_runner.py")
	scriptFile, err := os.Open(sparkScriptPath)
	if err != nil {
		return err
	}
	buff := make([]byte, 4096)
	_, err = scriptFile.Read(buff)
	if err != nil {
		return err
	}
	return store.Write(sparkScriptPath, buff)
}

type SparkGenericExecutor struct {
	master        string
	deployMode    string
	pythonVersion string
	logger        *zap.SugaredLogger
}

func (s *SparkGenericExecutor) InitializeExecutor(store FileStore) error {
	s.logger.Info("Uploading PySpark script to filestore")
	sparkScriptPath := helpers.GetEnv("SPARK_SCRIPT_PATH", "/scripts/offline_store_spark_runner.py")
	sparkScriptPathWithPrefix := store.PathWithPrefix(sparkScriptPath, false)

	err := readAndUploadFile(sparkScriptPath, sparkScriptPathWithPrefix, store)
	scriptExists, _ := store.Exists(sparkScriptPathWithPrefix)
	if err != nil && !scriptExists {
		return fmt.Errorf("could not upload spark script: Path: %s, Error: %v", sparkScriptPathWithPrefix, err)
	}
	return nil
}

func (s *SparkGenericExecutor) RunSparkJob(args []string, store FileStore) error {
	bashCommand := "bash"
	sparkArgsString := strings.Join(args, " ")
	bashCommandArgs := []string{"-c", fmt.Sprintf("pyenv global %s && pyenv exec %s", s.pythonVersion, sparkArgsString)}

	s.logger.Info("Executing spark-submit")
	cmd := exec.Command(bashCommand, bashCommandArgs...)
	cmd.Env = append(os.Environ(), "FEATUREFORM_LOCAL_MODE=true")

	err := cmd.Start()
	if err != nil {
		return fmt.Errorf("could not run spark job: %v", err)
	}

	err = cmd.Wait()
	if err != nil {
		return fmt.Errorf("spark job failed: %v", err)
	}

	return nil
}

func (s *SparkGenericExecutor) PythonFileURI(store FileStore) string {
	// not used for Spark Generic Executor
	return ""
}

func (s *SparkGenericExecutor) SparkSubmitArgs(destPath string, cleanQuery string, sourceList []string, jobType JobType, store FileStore) []string {
	sparkScriptPath := helpers.GetEnv("SPARK_SCRIPT_PATH", "/scripts/offline_store_spark_runner.py")

	argList := []string{
		"spark-submit",
		"--master",
		s.master,
		"--deploy-mode",
		s.deployMode,
	}
	scriptArgs := []string{
		sparkScriptPath,
		"sql",
		"--output_uri",
		fmt.Sprintf("\"%s\"", destPath),
		"--sql_query",
		fmt.Sprintf("\"%s\"", cleanQuery),
		"--job_type",
		fmt.Sprintf("\"%s\"", string(jobType)),
	}

	var packageArgs []string
	azureStore := store.AsAzureStore()

	if azureStore != nil {
		packageArgs = []string{
			"--packages",
			"\"org.apache.hadoop:hadoop-azure:3.2.0\"",
		}

		remoteConnectionArgs := []string{
			"--spark_config",
			fmt.Sprintf("\"%s\"", azureStore.configString()),
		}

		scriptArgs = append(scriptArgs, remoteConnectionArgs...)
	}

	argList = append(argList, packageArgs...) // adding any packages needed for filestores
	argList = append(argList, scriptArgs...)  // adding pyspark arguments

	argList = append(argList, "--source_list")
	for _, source := range sourceList {
		argList = append(argList, fmt.Sprintf("\"%s\"", source))
	}
	return argList
}

func (s *SparkGenericExecutor) GetDFArgs(outputURI string, code string, sources []string, store FileStore) ([]string, error) {
	sparkScriptPath := helpers.GetEnv("SPARK_SCRIPT_PATH", "/scripts/offline_store_spark_runner.py")

	argList := []string{
		"spark-submit",
		"--master",
		s.master,
		"--deploy-mode",
		s.deployMode,
	}

	scriptArgs := []string{
		sparkScriptPath,
		"df",
		"--output_uri",
		fmt.Sprintf("\"%s\"", outputURI),
		"--code",
		code,
	}

	var packageArgs []string
	azureStore := store.AsAzureStore()

	if azureStore != nil {
		packageArgs = []string{
			"--packages",
			"\"org.apache.hadoop:hadoop-azure:3.2.0\"",
		}

		remoteConnectionArgs := []string{
			"--store_type",
			"azure_blob_store",
			"--spark_config",
			fmt.Sprintf("\"%s\"", azureStore.configString()),
			"--credential",
			fmt.Sprintf("\"azure_connection_string=%s\"", azureStore.connectionString()),
			"--credential",
			fmt.Sprintf("\"azure_container_name=%s\"", azureStore.containerName()),
		}

		scriptArgs = append(scriptArgs, remoteConnectionArgs...)
	}

	argList = append(argList, packageArgs...) // adding any packages needed for filestores
	argList = append(argList, scriptArgs...)  // adding pyspark script arguments

	argList = append(argList, "--source")
	for _, source := range sources {
		argList = append(argList, fmt.Sprintf("\"%s\"", source))
	}

	return argList, nil
}

func NewSparkGenericExecutor(sparkGenericConfig pc.SparkGenericConfig, logger *zap.SugaredLogger) (SparkExecutor, error) {
	sparkGenericExecutor := SparkGenericExecutor{
		master:        sparkGenericConfig.Master,
		deployMode:    sparkGenericConfig.DeployMode,
		pythonVersion: sparkGenericConfig.PythonVersion,
		logger:        logger,
	}
	return &sparkGenericExecutor, nil
}

func NewSparkExecutor(execType pc.SparkExecutorType, config pc.SparkExecutorConfig, logger *zap.SugaredLogger) (SparkExecutor, error) {
	switch execType {
	case pc.EMR:
		emrConfig, ok := config.(*pc.EMRConfig)
		if !ok {
			return nil, fmt.Errorf("cannot convert config into 'EMRConfig'")
		}
		return NewEMRExecutor(*emrConfig, logger)
	case pc.Databricks:
		databricksConfig, ok := config.(*pc.DatabricksConfig)
		if !ok {
			return nil, fmt.Errorf("cannot convert config into 'DatabricksConfig'")
		}
		return NewDatabricksExecutor(*databricksConfig)
	case pc.SparkGeneric:
		sparkGenericConfig, ok := config.(*pc.SparkGenericConfig)
		if !ok {
			return nil, fmt.Errorf("cannot convert config into 'SparkGenericConfig'")
		}
		return NewSparkGenericExecutor(*sparkGenericConfig, logger)
	default:
		return nil, fmt.Errorf("the executor type ('%s') is not supported", execType)
	}
}

func NewEMRExecutor(emrConfig pc.EMRConfig, logger *zap.SugaredLogger) (SparkExecutor, error) {
	client := emr.New(emr.Options{
		Region:      emrConfig.ClusterRegion,
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider(emrConfig.Credentials.AWSAccessKeyId, emrConfig.Credentials.AWSSecretKey, "")),
	})

	emrExecutor := EMRExecutor{
		client:      client,
		logger:      logger,
		clusterName: emrConfig.ClusterName,
	}
	return &emrExecutor, nil
}

func (e *EMRExecutor) RunSparkJob(args []string, store FileStore) error {
	params := &emr.AddJobFlowStepsInput{
		JobFlowId: aws.String(e.clusterName), //returned by listclusters
		Steps: []emrTypes.StepConfig{
			{
				Name: aws.String("Featureform execution step"),
				HadoopJarStep: &emrTypes.HadoopJarStepConfig{
					Jar:  aws.String("command-runner.jar"), //jar file for running pyspark scripts
					Args: args,
				},
				ActionOnFailure: emrTypes.ActionOnFailureContinue,
			},
		},
	}
	resp, err := e.client.AddJobFlowSteps(context.TODO(), params)
	if err != nil {
		e.logger.Errorw("Could not add job flow steps to EMR cluster", err)
		return err
	}
	stepId := resp.StepIds[0]
	var waitDuration time.Duration = time.Second * 500
	e.logger.Debugw("Waiting for EMR job to complete")
	stepCompleteWaiter := emr.NewStepCompleteWaiter(e.client)
	_, err = stepCompleteWaiter.WaitForOutput(context.TODO(), &emr.DescribeStepInput{
		ClusterId: aws.String(e.clusterName),
		StepId:    aws.String(stepId),
	}, waitDuration)
	if err != nil {
		e.logger.Errorw("Failure waiting for completion of EMR cluster", err)
		return err
	}
	return nil
}

func (e *EMRExecutor) SparkSubmitArgs(destPath string, cleanQuery string, sourceList []string, jobType JobType, store FileStore) []string {
	argList := []string{
		"spark-submit",
		"--deploy-mode",
		"client",
		store.PathWithPrefix("featureform/scripts/offline_store_spark_runner.py", true),
		"sql",
		"--output_uri",
		store.PathWithPrefix(destPath, true),
		"--sql_query",
		cleanQuery,
		"--job_type",
		string(jobType),
		"--source_list",
	}
	argList = append(argList, sourceList...)
	return argList
}

func (d *DatabricksExecutor) SparkSubmitArgs(destPath string, cleanQuery string, sourceList []string, jobType JobType, store FileStore) []string {
	argList := []string{
		"sql",
		"--output_uri",
		destPath,
		"--sql_query",
		cleanQuery,
		"--job_type",
		string(jobType),
	}
	var remoteConnectionArgs []string
	azureStore := store.AsAzureStore()
	if azureStore != nil {
		remoteConnectionArgs = []string{
			"--spark_config",
			azureStore.configString(),
		}
	}
	argList = append(argList, remoteConnectionArgs...)

	argList = append(argList, "--source_list")
	argList = append(argList, sourceList...)
	return argList
}

func (spark *SparkOfflineStore) RegisterPrimaryFromSourceTable(id ResourceID, sourceName string) (PrimaryTable, error) {
	return blobRegisterPrimary(id, sourceName, spark.Logger, spark.Store)
}

func (spark *SparkOfflineStore) pysparkArgs(destinationURI string, templatedQuery string, sourceList []string, jobType JobType) *[]string {
	args := []string{}
	return &args
}

func (spark *SparkOfflineStore) RegisterResourceFromSourceTable(id ResourceID, schema ResourceSchema) (OfflineTable, error) {
	return blobRegisterResource(id, schema, spark.Logger, spark.Store)
}

func (spark *SparkOfflineStore) CreateTransformation(config TransformationConfig) error {
	return spark.transformation(config, false)
}

func (spark *SparkOfflineStore) transformation(config TransformationConfig, isUpdate bool) error {
	if config.Type == SQLTransformation {
		return spark.sqlTransformation(config, isUpdate)
	} else if config.Type == DFTransformation {
		return spark.dfTransformation(config, isUpdate)
	} else {
		spark.Logger.Errorw("Unsupported transformation type", config.Type)
		return fmt.Errorf("the transformation type '%v' is not supported", config.Type)
	}
}

func (spark *SparkOfflineStore) sqlTransformation(config TransformationConfig, isUpdate bool) error {
	updatedQuery, sources, err := spark.updateQuery(config.Query, config.SourceMapping)
	if err != nil {
		spark.Logger.Errorw("Could not generate updated query for spark transformation", err)
		return err
	}

	transformationDestination := spark.Store.PathWithPrefix(ResourcePrefix(config.TargetTableID), true)
	bucketTransformationDest := spark.Store.PathWithPrefix(ResourcePrefix(config.TargetTableID), false)
	newestTransformationFile, err := spark.Store.NewestFileOfType(bucketTransformationDest, Parquet)
	if err != nil {
		return fmt.Errorf("could not get newest transformation file: %v", err)
	}
	transformationExists := newestTransformationFile != ""
	if !isUpdate && transformationExists {
		spark.Logger.Errorw("Creation when transformation already exists", config.TargetTableID, transformationDestination)
		return fmt.Errorf("transformation %v already exists at %s", config.TargetTableID, transformationDestination)
	} else if isUpdate && !transformationExists {
		spark.Logger.Errorw("Update job attempted when transformation does not exist", config.TargetTableID, transformationDestination)
		return fmt.Errorf("transformation %v doesn't exist at %s and you are trying to update", config.TargetTableID, transformationDestination)
	}

	spark.Logger.Debugw("Running SQL transformation", config)
	sparkArgs := spark.Executor.SparkSubmitArgs(transformationDestination, updatedQuery, sources, JobType(Transform), spark.Store)
	if err := spark.Executor.RunSparkJob(sparkArgs, spark.Store); err != nil {
		spark.Logger.Errorw("spark submit job for transformation failed to run", config.TargetTableID, err)
		return fmt.Errorf("spark submit job for transformation %v failed to run: %v", config.TargetTableID, err)
	}
	spark.Logger.Debugw("Succesfully ran SQL transformation", config)
	return nil
}

func GetTransformationFileLocation(id ResourceID) string {
	return fmt.Sprintf("featureform/DFTranformations/%s/%s", id.Name, id.Variant)
}

func (spark *SparkOfflineStore) dfTransformation(config TransformationConfig, isUpdate bool) error {
	transformationDestination := spark.Store.PathWithPrefix(ResourcePrefix(config.TargetTableID), true)
	transformationDestinationWithSlash := strings.Join([]string{transformationDestination, ""}, "/")

	transformationFile, err := spark.Store.NewestFileOfType(spark.Store.PathWithPrefix(ResourcePrefix(config.TargetTableID), false), Parquet)
	if err != nil {
		return fmt.Errorf("error checking if transformation file exists")
	}
	transformationExists := transformationFile != ""
	if !isUpdate && transformationExists {
		spark.Logger.Errorw("Transformation already exists", config.TargetTableID, transformationDestination)
		return fmt.Errorf("transformation %v already exists at %s", config.TargetTableID, transformationDestination)
	} else if isUpdate && !transformationExists {
		spark.Logger.Errorw("Transformation doesn't exists at destination and you are trying to update", config.TargetTableID, transformationDestination)
		return fmt.Errorf("transformation %v doesn't exist at %s and you are trying to update", config.TargetTableID, transformationDestination)
	}

	transformationFilePath := GetTransformationFileLocation(config.TargetTableID)
	fileName := "transformation.pkl"
	transformationFileLocation := fmt.Sprintf("%s/%s", transformationFilePath, fileName)

	if err := spark.Store.Write(transformationFileLocation, config.Code); err != nil {
		return fmt.Errorf("could not upload file: %s", err)
	}

	sources, err := spark.getSources(config.SourceMapping)
	if err != nil {
		return fmt.Errorf("could not get sources for df transformation. Error: %v", err)
	}

	sparkArgs, err := spark.Executor.GetDFArgs(transformationDestinationWithSlash, transformationFileLocation, sources, spark.Store)
	if err != nil {
		spark.Logger.Errorw("Problem creating spark dataframe arguments", err)
		return fmt.Errorf("error with getting df arguments %v", sparkArgs)
	}
	spark.Logger.Debugw("Running DF transformation")
	if err := spark.Executor.RunSparkJob(sparkArgs, spark.Store); err != nil {
		spark.Logger.Errorw("Error running Spark dataframe job", err)
		return fmt.Errorf("spark submit job for transformation failed to run: (name: %s variant:%s) %v", config.TargetTableID.Name, config.TargetTableID.Variant, err)
	}
	spark.Logger.Debugw("Successfully ran transformation", "type", config.Type, "name", config.TargetTableID.Name, "variant", config.TargetTableID.Variant)
	return nil
}

func (spark *SparkOfflineStore) getSources(mapping []SourceMapping) ([]string, error) {
	sources := []string{}

	for _, m := range mapping {
		sourcePath, err := spark.getSourcePath(m.Source)
		if err != nil {
			spark.Logger.Errorw("Error getting source path for spark source", m.Source, err)
			return nil, fmt.Errorf("issue with retreiving the source path for %s because %s", m.Source, err)
		}

		sources = append(sources, sourcePath)
	}
	return sources, nil
}

func (spark *SparkOfflineStore) updateQuery(query string, mapping []SourceMapping) (string, []string, error) {
	sources := make([]string, len(mapping))
	replacements := make([]string, len(mapping)*2) // It's times 2 because each replacement will be a pair; (original, replacedValue)

	for i, m := range mapping {
		replacements = append(replacements, m.Template)
		replacements = append(replacements, fmt.Sprintf("source_%v", i))

		sourcePath, err := spark.getSourcePath(m.Source)
		if err != nil {
			spark.Logger.Errorw("Error getting source path of spark source", m.Source, err)
			return "", nil, fmt.Errorf("could not get the sourcePath for %s because %s", m.Source, err)
		}

		sources[i] = sourcePath
	}

	replacer := strings.NewReplacer(replacements...)
	updatedQuery := replacer.Replace(query)

	if strings.Contains(updatedQuery, "{{") {
		spark.Logger.Errorw("Template replace failed", updatedQuery)
		return "", nil, fmt.Errorf("could not replace all the templates with the current mapping. Mapping: %v; Replaced Query: %s", mapping, updatedQuery)
	}
	return updatedQuery, sources, nil
}

func (spark *SparkOfflineStore) getSourcePath(path string) (string, error) {
	fileType, fileName, fileVariant := spark.getResourceInformationFromFilePath(path)

	var filePath string
	if fileType == "primary" {
		fileResourceId := ResourceID{Name: fileName, Variant: fileVariant, Type: Primary}
		fileTable, err := spark.GetPrimaryTable(fileResourceId)
		if err != nil {
			spark.Logger.Errorw("Issue getting primary table", fileResourceId, err)
			return "", fmt.Errorf("could not get the primary table for {%v} because %s", fileResourceId, err)
		}
		filePath = fileTable.GetName()
		return filePath, nil
	} else if fileType == "transformation" {
		fileResourceId := ResourceID{Name: fileName, Variant: fileVariant, Type: Transformation}

		transformationPath, err := spark.Store.NewestFileOfType(spark.Store.PathWithPrefix(ResourcePrefix(fileResourceId), false), Parquet)
		if err != nil || transformationPath == "" {
			return "", fmt.Errorf("could not get transformation file path: %v", err)
		}

		filePath = spark.Store.PathWithPrefix(transformationPath[:strings.LastIndex(transformationPath, "/")], true)
		return filePath, nil
	} else {
		return filePath, fmt.Errorf("could not find path for %s; fileType: %s, fileName: %s, fileVariant: %s", path, fileType, fileName, fileVariant)
	}
}

func (spark *SparkOfflineStore) getResourceInformationFromFilePath(path string) (string, string, string) {
	var fileType string
	var fileName string
	var fileVariant string
	containsSlashes := strings.Contains(path, "/")
	if path[:5] == "s3://" {
		filePaths := strings.Split(path[len("s3://"):], "/")
		if len(filePaths) <= 4 {
			return "", "", ""
		}
		fileType, fileName, fileVariant = strings.ToLower(filePaths[2]), filePaths[3], filePaths[4]
	} else if containsSlashes {
		filePaths := strings.Split(path[len("featureform/"):], "/")
		if len(filePaths) <= 2 {
			return "", "", ""
		}
		fileType, fileName, fileVariant = strings.ToLower(filePaths[0]), filePaths[1], filePaths[2]
	} else {
		filePaths := strings.Split(path[len("featureform_"):], "__")
		if len(filePaths) <= 2 {
			return "", "", ""
		}
		fileType, fileName, fileVariant = filePaths[0], filePaths[1], filePaths[2]
	}
	return fileType, fileName, fileVariant
}

func (e *EMRExecutor) GetDFArgs(outputURI string, code string, sources []string, store FileStore) ([]string, error) {
	argList := []string{
		"spark-submit",
		"--deploy-mode",
		"client",
		store.PathWithPrefix("featureform/scripts/offline_store_spark_runner.py", true),
		"df",
		"--output_uri",
		outputURI,
		"--code",
		store.PathWithPrefix(code, true),
		"--source",
	}

	argList = append(argList, sources...)

	return argList, nil
}

func (d *DatabricksExecutor) GetDFArgs(outputURI string, code string, sources []string, store FileStore) ([]string, error) {
	argList := []string{
		"df",
		"--output_uri",
		outputURI,
		"--code",
		code,
	}
	var remoteConnectionArgs []string
	azureStore := store.AsAzureStore()

	if azureStore != nil {
		remoteConnectionArgs = []string{
			"--store_type",
			"azure_blob_store",
			"--spark_config",
			azureStore.configString(),
			"--credential",
			fmt.Sprintf("azure_connection_string=%s", azureStore.connectionString()),
			"--credential",
			fmt.Sprintf("azure_container_name=%s", azureStore.containerName()),
		}
	}
	argList = append(argList, remoteConnectionArgs...)

	argList = append(argList, "--source")
	argList = append(argList, sources...)

	return argList, nil
}

func (spark *SparkOfflineStore) GetTransformationTable(id ResourceID) (TransformationTable, error) {
	spark.Logger.Debugw("Getting transformation table", "ResourceID", id)
	transformationPath := spark.Store.PathWithPrefix(fileStoreResourcePath(id), false)
	transformationExactPath, err := spark.Store.NewestFileOfType(spark.Store.PathWithPrefix(transformationPath, false), Parquet)
	fmt.Println("GetTransformation", transformationPath, transformationExactPath)
	if err != nil || transformationExactPath == "" {
		return nil, fmt.Errorf("could not get transformation table: %v", err)
	}
	spark.Logger.Debugw("Succesfully retrieved transformation table", "ResourceID", id)
	return &FileStorePrimaryTable{spark.Store, transformationExactPath, true, id}, nil
}

func (spark *SparkOfflineStore) UpdateTransformation(config TransformationConfig) error {
	return spark.transformation(config, true)
}

func (spark *SparkOfflineStore) CreatePrimaryTable(id ResourceID, schema TableSchema) (PrimaryTable, error) {
	return nil, nil
}

func (spark *SparkOfflineStore) GetPrimaryTable(id ResourceID) (PrimaryTable, error) {
	return fileStoreGetPrimary(id, spark.Store, spark.Logger)
}

func (spark *SparkOfflineStore) CreateResourceTable(id ResourceID, schema TableSchema) (OfflineTable, error) {
	return nil, nil
}

func (spark *SparkOfflineStore) GetResourceTable(id ResourceID) (OfflineTable, error) {
	return fileStoreGetResourceTable(id, spark.Store, spark.Logger)
}

func blobSparkMaterialization(id ResourceID, spark *SparkOfflineStore, isUpdate bool) (Materialization, error) {
	if id.Type != Feature {
		spark.Logger.Errorw("Attempted to create a materialization of a non feature resource", id.Type)
		return nil, fmt.Errorf("only features can be materialized")
	}
	resourceTable, err := spark.GetResourceTable(id)
	if err != nil {
		spark.Logger.Errorw("Attempted to fetch resource table of non registered resource", err)
		return nil, fmt.Errorf("resource not registered: %v", err)
	}
	sparkResourceTable, ok := resourceTable.(*BlobOfflineTable)
	if !ok {
		spark.Logger.Errorw("Could not convert resource table to S3 offline table", id)
		return nil, fmt.Errorf("could not convert offline table with id %v to sparkResourceTable", id)
	}
	materializationID := ResourceID{Name: id.Name, Variant: id.Variant, Type: FeatureMaterialization}
	destinationPath := spark.Store.PathWithPrefix(ResourcePrefix(materializationID), true)
	materializationNewestFile, err := spark.Store.NewestFileOfType(spark.Store.PathWithPrefix(fileStoreResourcePath(materializationID), false), Parquet)
	if err != nil {
		return nil, fmt.Errorf("could not get newest materialization file: %v", err)
	}
	materializationExists := materializationNewestFile != ""
	if materializationExists && !isUpdate {
		spark.Logger.Errorw("Attempted to materialize a materialization that already exists", id)
		return nil, fmt.Errorf("materialization already exists")
	} else if !materializationExists && isUpdate {
		spark.Logger.Errorw("Attempted to materialize a materialization that already exists", id)
		return nil, fmt.Errorf("materialization already exists")
	}
	materializationQuery := spark.query.materializationCreate(sparkResourceTable.schema)
	sourcePath := spark.Store.PathWithPrefix(sparkResourceTable.schema.SourceTable, true)
	sparkArgs := spark.Executor.SparkSubmitArgs(destinationPath, materializationQuery, []string{sourcePath}, Materialize, spark.Store)
	spark.Logger.Debugw("Creating materialization", "id", id)
	if err := spark.Executor.RunSparkJob(sparkArgs, spark.Store); err != nil {
		spark.Logger.Errorw("Spark submit job failed to run", "error", err)
		return nil, fmt.Errorf("spark submit job for materialization %v failed to run: %v", materializationID, err)
	}
	key, err := spark.Store.NewestFileOfType(spark.Store.PathWithPrefix(fileStoreResourcePath(materializationID), false), Parquet)
	if err != nil || key == "" {
		return nil, fmt.Errorf("could not get newest materialization file: %v", err)
	}
	spark.Logger.Debugw("Successfully created materialization", "id", id)
	return &FileStoreMaterialization{materializationID, spark.Store, key}, nil
}

func (spark *SparkOfflineStore) CreateMaterialization(id ResourceID) (Materialization, error) {
	return blobSparkMaterialization(id, spark, false)
}

func (spark *SparkOfflineStore) GetMaterialization(id MaterializationID) (Materialization, error) {
	return fileStoreGetMaterialization(id, spark.Store, spark.Logger)
}

func (spark *SparkOfflineStore) UpdateMaterialization(id ResourceID) (Materialization, error) {
	return blobSparkMaterialization(id, spark, true)
}

func (spark *SparkOfflineStore) DeleteMaterialization(id MaterializationID) error {
	return fileStoreDeleteMaterialization(id, spark.Store, spark.Logger)
}

func (spark *SparkOfflineStore) registeredResourceSchema(id ResourceID) (ResourceSchema, error) {
	spark.Logger.Debugw("Getting resource schema", "id", id)
	table, err := spark.GetResourceTable(id)
	if err != nil {
		spark.Logger.Errorw("Resource not registered in spark store", id, err)
		return ResourceSchema{}, fmt.Errorf("resource not registered: %v", err)
	}
	sparkResourceTable, ok := table.(*BlobOfflineTable)
	if !ok {
		spark.Logger.Errorw("could not convert offline table to sparkResourceTable", id)
		return ResourceSchema{}, fmt.Errorf("could not convert offline table with id %v to sparkResourceTable", id)
	}
	spark.Logger.Debugw("Succesfully retrieved resource schema", "id", id)
	return sparkResourceTable.schema, nil
}

func sparkTrainingSet(def TrainingSetDef, spark *SparkOfflineStore, isUpdate bool) error {
	if err := def.check(); err != nil {
		spark.Logger.Errorw("Training set definition not valid", def, err)
		return err
	}
	sourcePaths := make([]string, 0)
	featureSchemas := make([]ResourceSchema, 0)
	destinationPath := spark.Store.PathWithPrefix(ResourcePrefix(def.ID), true)
	trainingSetNewestFile, err := spark.Store.NewestFileOfType(spark.Store.PathWithPrefix(fileStoreResourcePath(def.ID), false), Parquet)
	if err != nil {
		return fmt.Errorf("Error getting training set newest file: %v", err)
	}
	trainingSetExists := trainingSetNewestFile != ""
	if trainingSetExists && !isUpdate {
		spark.Logger.Errorw("Training set already exists", "id", def.ID)
		return fmt.Errorf("spark training set already exists: %v", def.ID)
	} else if !trainingSetExists && isUpdate {
		spark.Logger.Errorw("Training set does not exist", "id", def.ID)
		return fmt.Errorf("spark training set does not exist: %v", def.ID)
	}
	labelSchema, err := spark.registeredResourceSchema(def.Label)
	if err != nil {
		spark.Logger.Errorw("Could not get schema of label in spark store", def.Label, err)
		return fmt.Errorf("could not get schema of label %s: %v", def.Label, err)
	}
	labelPath := spark.Store.PathWithPrefix(labelSchema.SourceTable, true)
	sourcePaths = append(sourcePaths, labelPath)
	for _, feature := range def.Features {
		featureSchema, err := spark.registeredResourceSchema(feature)
		if err != nil {
			spark.Logger.Errorw("Could not get schema of feature in spark store", feature, err)
			return fmt.Errorf("could not get schema of feature %s: %v", feature, err)
		}
		featurePath := spark.Store.PathWithPrefix(featureSchema.SourceTable, true)
		sourcePaths = append(sourcePaths, featurePath)
		featureSchemas = append(featureSchemas, featureSchema)
	}
	trainingSetQuery := spark.query.trainingSetCreate(def, featureSchemas, labelSchema)
	sparkArgs := spark.Executor.SparkSubmitArgs(destinationPath, trainingSetQuery, sourcePaths, CreateTrainingSet, spark.Store)
	spark.Logger.Debugw("Creating training set", "definition", def)
	if err := spark.Executor.RunSparkJob(sparkArgs, spark.Store); err != nil {
		spark.Logger.Errorw("Spark submit training set job failed to run", "definition", def.ID, "error", err)
		return fmt.Errorf("spark submit job for training set %v failed to run: %v", def.ID, err)
	}
	newestTrainingSet, err := spark.Store.NewestFileOfType(spark.Store.PathWithPrefix(ResourcePrefix(def.ID), false), Parquet)
	if err != nil {
		return fmt.Errorf("could not check that training set was created: %v", err)
	}
	if newestTrainingSet == "" {
		spark.Logger.Errorw("Could not get training set resource key in offline store")
		return fmt.Errorf("training Set result does not exist in offline store")
	}
	spark.Logger.Debugw("Succesfully created training set:", "definition", def)
	return nil
}

func (spark *SparkOfflineStore) CreateTrainingSet(def TrainingSetDef) error {
	return sparkTrainingSet(def, spark, false)

}

func (spark *SparkOfflineStore) UpdateTrainingSet(def TrainingSetDef) error {
	return sparkTrainingSet(def, spark, true)
}

func (spark *SparkOfflineStore) GetTrainingSet(id ResourceID) (TrainingSetIterator, error) {
	return fileStoreGetTrainingSet(id, spark.Store, spark.Logger)
}

func sanitizeSparkSQL(name string) string {
	return name
}

func ResourcePath(id ResourceID) string {
	return fmt.Sprintf("%s/%s/%s", id.Type, id.Name, id.Variant)
}
