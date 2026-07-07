package models_test

import (
	"encoding/json"
	"go-stock/backend/db"
	"go-stock/backend/logger"
	"go-stock/backend/models"
	"os"
	"testing"

	"github.com/duke-git/lancet/v2/strutil"
)

// @Author spark
// @Date 2025/2/22 16:09
// @Desc
// -----------------------------------------------------------------------------------
type StockInfoHKResp struct {
	Code       int              `json:"code"`
	Status     string           `json:"status"`
	StockInfos *[]StockInfoData `json:"data"`
}

type StockInfoData struct {
	C string `json:"c"`
	N string `json:"n"`
	T string `json:"t"`
	E string `json:"e"`
}

func TestStockInfoHK(t *testing.T) {
	db.Init("../../data/stock.db")
	db.Dao.AutoMigrate(&models.StockInfoHK{})
	bs, _ := os.ReadFile("../../build/hk.json")
	v := &StockInfoHKResp{}
	err := json.Unmarshal(bs, v)
	if err != nil {
		return
	}
	hks := &[]models.StockInfoHK{}
	for i, data := range *v.StockInfos {
		logger.SugaredLogger.Infof("第%d条数据: %+v", i, data)
		hk := &models.StockInfoHK{
			Code:  strutil.PadStart(data.C, 5, "0") + ".HK",
			EName: data.N,
		}
		*hks = append(*hks, *hk)
	}
	db.Dao.Create(&hks)

}
