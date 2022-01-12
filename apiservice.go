package portto

import (
	"fmt"
	"log"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

func NewAPIService(i *Indexer) *APIService {
	return &APIService{indexer: i}
}

type APIService struct {
	indexer *Indexer
}

func (s APIService) ListenAndServe(addr string) {
	r := gin.Default()
	r.GET("/blocks", s.blocksHandler)
	err := r.Run(addr)
	if err != nil {
		log.Fatal("failed to run http server, ", err)
	}
}

func (s APIService) blocksHandler(c *gin.Context) {
	limitRaw := c.DefaultQuery("limit", "1")
	limit, err := strconv.Atoi(limitRaw)
	if err != nil || limit < 1 || limit > 10 {
		c.JSON(http.StatusBadRequest, gin.H{
			"message": "limit must be a number and between 1 and 10",
		})
		return
	}

	blocks, err := s.indexer.GetNewBlocks(limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"message": fmt.Sprintf("error: %v", err),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"blocks": blocks,
	})
}
