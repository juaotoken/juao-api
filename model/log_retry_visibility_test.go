package model

import (
	"os"
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// useRetryVisibilityLogDB 把 LOG_DB 切到指定方言的库并建好 logs 表。
// mysql / postgres 需要 TEST_MYSQL_DSN / TEST_POSTGRES_DSN，未配置时跳过。
func useRetryVisibilityLogDB(t *testing.T, dialect string) {
	t.Helper()
	var driver gorm.Dialector
	dbType := common.DatabaseTypeSQLite
	switch dialect {
	case "sqlite":
		driver = sqlite.Open(":memory:")
	case "mysql":
		dsn := os.Getenv("TEST_MYSQL_DSN")
		if dsn == "" {
			t.Skip("TEST_MYSQL_DSN is not configured")
		}
		driver, dbType = mysql.Open(dsn), common.DatabaseTypeMySQL
	case "postgres":
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("TEST_POSTGRES_DSN is not configured")
		}
		driver, dbType = postgres.Open(dsn), common.DatabaseTypePostgreSQL
	}
	db, err := gorm.Open(driver, &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.Migrator().DropTable(&Log{}))
	require.NoError(t, db.AutoMigrate(&Log{}))

	previousLogDB, previousMain, previousLog := LOG_DB, common.MainDatabaseType(), common.LogDatabaseType()
	LOG_DB = db
	common.SetDatabaseTypes(previousMain, dbType)
	initCol()
	t.Cleanup(func() {
		_ = db.Migrator().DropTable(&Log{})
		_ = sqlDB.Close()
		LOG_DB = previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		initCol()
	})
}

// 线上两个真实请求的形态：
//   - req-retry-ok：一次 503 失败 → 重试成功（1 条错误 + 1 条消费）
//   - req-retry-fail：重试 2 次全部失败（3 条错误，常落在同一秒）
//
// 普通用户只能看到每个请求的最终结果；管理员看全部尝试。
func TestGetUserLogsHidesRetriedErrorAttempts(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			useRetryVisibilityLogDB(t, dialect)
			const userId, otherUserId, tokenId = 50, 51, 9
			rows := []Log{
				{UserId: userId, TokenId: tokenId, Type: LogTypeError, CreatedAt: 100, RequestId: "req-retry-fail", Content: "status_code=400, first"},
				{UserId: userId, TokenId: tokenId, Type: LogTypeError, CreatedAt: 113, RequestId: "req-retry-fail", Content: "status_code=400, second"},
				{UserId: userId, TokenId: tokenId, Type: LogTypeError, CreatedAt: 113, RequestId: "req-retry-fail", Content: "status_code=400, final"},
				{UserId: userId, TokenId: tokenId, Type: LogTypeError, CreatedAt: 200, RequestId: "req-retry-ok", Content: "status_code=503, retried"},
				{UserId: userId, TokenId: tokenId, Type: LogTypeConsume, CreatedAt: 262, RequestId: "req-retry-ok", Content: "consume"},
				{UserId: userId, TokenId: tokenId, Type: LogTypeError, CreatedAt: 300, RequestId: "req-single-fail", Content: "status_code=500, only"},
				// 他人的同 request_id 日志不能把本用户的失败「挤掉」。
				{UserId: otherUserId, Type: LogTypeConsume, CreatedAt: 301, RequestId: "req-single-fail", Content: "other user"},
				{UserId: userId, TokenId: tokenId, Type: LogTypeTopup, CreatedAt: 400, Content: "topup"},
			}
			for i := range rows {
				require.NoError(t, LOG_DB.Create(&rows[i]).Error)
			}
			contents := func(logs []*Log) []string {
				result := make([]string, 0, len(logs))
				for _, log := range logs {
					result = append(result, log.Content)
				}
				return result
			}

			userLogs, total, err := GetUserLogs(userId, LogTypeUnknown, 0, 0, "", "", 0, 50, "", "", "", true)
			require.NoError(t, err)
			assert.Equal(t, []string{"topup", "status_code=500, only", "consume", "status_code=400, final"}, contents(userLogs))
			assert.EqualValues(t, 4, total, "分页总数必须与过滤后的行数一致")

			errorLogs, total, err := GetUserLogs(userId, LogTypeError, 0, 0, "", "", 0, 50, "", "", "", true)
			require.NoError(t, err)
			assert.Equal(t, []string{"status_code=500, only", "status_code=400, final"}, contents(errorLogs))
			assert.EqualValues(t, 2, total)

			byRequest, _, err := GetUserLogs(userId, LogTypeUnknown, 0, 0, "", "", 0, 50, "", "req-retry-ok", "", true)
			require.NoError(t, err)
			assert.Equal(t, []string{"consume"}, contents(byRequest), "按 request_id 查也只看到重试后的成功日志")

			adminView, total, err := GetUserLogs(userId, LogTypeError, 0, 0, "", "", 0, 50, "", "", "", false)
			require.NoError(t, err)
			assert.Len(t, adminView, 5, "管理员看到全部失败尝试")
			assert.EqualValues(t, 5, total)

			tokenLogs, err := GetLogByTokenId(tokenId)
			require.NoError(t, err)
			assert.Equal(t, []string{"topup", "status_code=500, only", "consume", "status_code=400, final"}, contents(tokenLogs))
		})
	}
}

func TestErrorLogRawRequestBodyLimitOption(t *testing.T) {
	previousLimit, previousMap := common.ErrorLogRawRequestBodyLimit, common.OptionMap
	common.OptionMap = map[string]string{}
	t.Cleanup(func() { common.ErrorLogRawRequestBodyLimit, common.OptionMap = previousLimit, previousMap })

	for _, value := range []string{"0", "1000", "5000", "1000000"} {
		assert.NoError(t, validateOptionValue("ErrorLogRawRequestBodyLimit", value), value)
	}
	for _, value := range []string{"-1", "1000001", "abc", ""} {
		assert.Error(t, validateOptionValue("ErrorLogRawRequestBodyLimit", value), value)
	}

	require.NoError(t, updateOptionMap("ErrorLogRawRequestBodyLimit", "5000"))
	assert.Equal(t, 5000, common.ErrorLogRawRequestBodyLimit)
}
