package router

import (
	"github.com/QuantumNous/new-api/controller"
	"github.com/QuantumNous/new-api/middleware"

	"github.com/gin-gonic/gin"
)

func SetVideoRouter(router *gin.Engine) {
	videoSharedRouter := router.Group("/v1")
	videoSharedRouter.Use(middleware.RouteTag("relay"))
	videoSharedRouter.Use(middleware.TokenAuth())
	videoSharedRouter.Use(middleware.SystemPerformanceCheck())
	videoSharedRouter.POST(
		"/video/generations",
		middleware.VolcAssetChannelPin(),
		middleware.PinTaskPluginEndpoint(),
		middleware.TaskPluginEndpointOnly(middleware.ModelRequestRateLimit()),
		middleware.PrepareTaskPluginEndpoint(),
		middleware.Distribute(),
		func(c *gin.Context) {
			controller.RelayTaskPluginEndpoint(c, controller.RelayTask)
		},
	)

	// The task list selects no channel, so it skips Distribute (whose token
	// model-limit check would reject the model-less request).
	videoListRouter := router.Group("/v1")
	videoListRouter.Use(middleware.RouteTag("relay"), middleware.TokenAuth())
	videoListRouter.GET("/video/generations", controller.RelayVolcVideoList)

	videoV1Router := router.Group("/v1")
	videoV1Router.Use(middleware.RouteTag("relay"))
	videoV1Router.Use(middleware.TokenAuth(), middleware.Distribute())
	{
		videoV1Router.GET("/video/generations/:task_id", controller.RelayTaskFetch)
		videoV1Router.POST("/videos/:video_id/remix", controller.RelayTask)
	}
}

func SetVolcAssetRouter(router *gin.Engine) {
	assetRouter := router.Group("/v1/volcengine/assets")
	assetRouter.Use(middleware.RouteTag("relay"), middleware.TokenAuth(), middleware.SystemPerformanceCheck())
	{
		assetRouter.POST("", controller.VolcAssetCreate)
		assetRouter.GET("", controller.VolcAssetList)
		assetRouter.POST("/auth-sessions", controller.VolcAssetAuthSession)
		assetRouter.POST("/groups/by-byted-token", controller.VolcAssetGroupByBytedToken)
		assetRouter.GET("/:logical_id", controller.VolcAssetDetail)
		assetRouter.DELETE("/:logical_id", controller.VolcAssetDelete)
	}
}
