package service

import (
	"crypto/subtle"
	"errors"
	"strings"

	"github.com/mhsanaei/3x-ui/v2/database"
	"github.com/mhsanaei/3x-ui/v2/database/model"
	"github.com/mhsanaei/3x-ui/v2/util/common"
	"github.com/mhsanaei/3x-ui/v2/util/crypto"
	"github.com/mhsanaei/3x-ui/v2/util/random"
)

type ApiTokenService struct{}

const apiTokenLength = 48

type ApiTokenView struct {
	Id        int    `json:"id"`
	Name      string `json:"name"`
	Token     string `json:"token,omitempty"`
	Enabled   bool   `json:"enabled"`
	CreatedAt int64  `json:"createdAt"`
}

func toApiTokenView(t *model.ApiToken) *ApiTokenView {
	return &ApiTokenView{
		Id:        t.Id,
		Name:      t.Name,
		Enabled:   t.Enabled,
		CreatedAt: t.CreatedAt,
	}
}

func (s *ApiTokenService) List() ([]*ApiTokenView, error) {
	db := database.GetDB()
	var rows []*model.ApiToken
	if err := db.Model(model.ApiToken{}).Order("id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]*ApiTokenView, 0, len(rows))
	for _, row := range rows {
		out = append(out, toApiTokenView(row))
	}
	return out, nil
}

func (s *ApiTokenService) Create(name string) (*ApiTokenView, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, common.NewError("token name is required")
	}
	if len(name) > 64 {
		return nil, common.NewError("token name must be 64 characters or fewer")
	}

	db := database.GetDB()
	var count int64
	if err := db.Model(model.ApiToken{}).Where("name = ?", name).Count(&count).Error; err != nil {
		return nil, err
	}
	if count > 0 {
		return nil, common.NewError("a token with that name already exists")
	}

	plaintext := random.Seq(apiTokenLength)
	row := &model.ApiToken{
		Name:    name,
		Token:   crypto.HashTokenSHA256(plaintext),
		Enabled: true,
	}
	if err := db.Create(row).Error; err != nil {
		return nil, err
	}

	view := toApiTokenView(row)
	view.Token = plaintext
	return view, nil
}

func (s *ApiTokenService) Delete(id int) error {
	if id <= 0 {
		return common.NewError("invalid token id")
	}
	return database.GetDB().Where("id = ?", id).Delete(model.ApiToken{}).Error
}

func (s *ApiTokenService) SetEnabled(id int, enabled bool) error {
	if id <= 0 {
		return common.NewError("invalid token id")
	}
	res := database.GetDB().Model(model.ApiToken{}).Where("id = ?", id).Update("enabled", enabled)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return errors.New("token not found")
	}
	return nil
}

func (s *ApiTokenService) Match(presented string) bool {
	if presented == "" {
		return false
	}
	var rows []*model.ApiToken
	if err := database.GetDB().Model(model.ApiToken{}).Where("enabled = ?", true).Find(&rows).Error; err != nil {
		return false
	}
	presentedHash := []byte(crypto.HashTokenSHA256(presented))
	matched := false
	for _, row := range rows {
		if subtle.ConstantTimeCompare([]byte(row.Token), presentedHash) == 1 {
			matched = true
		}
	}
	return matched
}
