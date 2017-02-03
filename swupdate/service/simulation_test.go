package swupdate

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/dedis/onet.v1"
	"gopkg.in/dedis/onet.v1/log"
)

func TestInitializePackages(t *testing.T) {
	local := onet.NewLocalTest()
	defer local.CloseAll()
	_, roster, s := local.MakeHELS(5, swupdateService)
	service := s.(*Service)

	log.Lvl1("Reading all packages")
	pkgs, err := InitializePackages("snapshot/updates.csv", service, roster, 3, 10)
	log.ErrFatal(err)
	require.True(t, len(pkgs) > 0)
	for _, p := range pkgs {
		log.Lvl2("Searching package", p)
		pscRet, err := service.PackageSC(nil, &PackageSC{p})
		log.ErrFatal(err)
		psc := pscRet.(*PackageSCRet)
		assert.NotEqual(t, psc.First.Hash, psc.Last.Hash)
	}
}
