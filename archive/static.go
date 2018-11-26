package archive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/thisisaaronland/go-flickr-archive/flickr"
	"github.com/thisisaaronland/go-flickr-archive/user"
	"github.com/thisisaaronland/go-flickr-archive/util"
	"github.com/tidwall/gjson"
	_ "log"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type StaticArchive struct {
	Archive
	User user.User
	API  flickr.API
	Root string
}

func NewStaticArchiveForUser(api flickr.API, u user.User, root string) (Archive, error) {

	info, err := os.Stat(root)

	if os.IsNotExist(err) {
		return nil, err
	}

	if !info.IsDir() {
		return nil, errors.New("Archive root is not a directory")
	}

	archive := StaticArchive{
		User: u,
		API:  api,
		Root: "",
	}

	return &archive, nil
}

func (archive *StaticArchive) ArchivePhotosForDay(dt time.Time) error {

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// because time.Format() is just so weird...

	y, m, d := dt.Date()
	ymd := fmt.Sprintf("%04d-%02d-%02d", y, m, d)

	min_date := fmt.Sprintf("%s 00:00:00", ymd)
	max_date := fmt.Sprintf("%s 23:59:59", ymd)

	user_id := archive.User.ID()

	page := 1
	pages := 0

	for {

		params := url.Values{}
		params.Set("min_upload_date", min_date)
		params.Set("max_upload_date", max_date)
		params.Set("user_id", user_id)
		params.Set("page", strconv.Itoa(page))

		rsp, err := archive.API.ExecuteMethod("flickr.people.getPhotos", params)

		if err != nil {
			return err
		}

		var spr flickr.StandardPhotoResponse

		err = json.Unmarshal(rsp, &spr)

		if err != nil {
			return err
		}

		for _, ph := range spr.Photos.Photos {

			err = archive.ArchivePhoto(ctx, ph)

			if err != nil {
				return err
			}
		}

		pages = spr.Photos.Pages

		if pages == 0 || pages == page {
			break
		}

		page += 1
	}

	return nil
}

func (archive *StaticArchive) ArchivePhoto(ctx context.Context, ph flickr.StandardPhotoResponsePhoto) error {

	// https://www.flickr.com/services/api/flickr.photos.getInfo.html

	info_params := url.Values{}
	info_params.Set("photo_id", ph.ID)
	info_params.Set("secret", ph.Secret)

	info, info_err := archive.API.ExecuteMethod("flickr.photos.getInfo", info_params)

	if info_err != nil {
		return info_err
	}

	photo_id := gjson.GetBytes(info, "photo.id")

	if !photo_id.Exists() {
		return errors.New("Unable to determin photo ID")
	}

	date_taken := gjson.GetBytes(info, "photo.dates.taken")

	if !date_taken.Exists() {
		return errors.New("Unable to determine date taken")
	}

	ymd_taken := strings.Split(date_taken.String(), " ")
	ymd := ymd_taken[0]

	dt, err := time.Parse("2006-01-02", ymd)

	if err != nil {
		return err
	}

	is_public := gjson.GetBytes(info, "photo.visibility.ispublic")

	if !is_public.Exists() {
		return errors.New("Unable to determine visibility")
	}

	visibility := "private" // default

	if is_public.Bool() {
		visibility = "public"
	}

	var secret gjson.Result

	if visibility == "public" {
		secret = gjson.GetBytes(info, "photo.secret")
	} else {
		secret = gjson.GetBytes(info, "photo.originalsecret") // is that right?
	}

	if !secret.Exists() {
		return errors.New("Unable to determine secret")
	}

	username := archive.User.Username()

	root := filepath.Join(archive.Root, username)
	root = filepath.Join(root, visibility)
	root = filepath.Join(root, fmt.Sprintf("%04d", dt.Year()))
	root = filepath.Join(root, fmt.Sprintf("%02d", dt.Month()))
	root = filepath.Join(root, fmt.Sprintf("%02d", dt.Day()))
	root = filepath.Join(root, fmt.Sprintf("%d", photo_id.Int()))

	info_fname := fmt.Sprintf("%d_%s_i.json", photo_id.Int(), secret.String())
	info_path := filepath.Join(root, info_fname)

	_, err = os.Stat(root)

	if os.IsNotExist(err) {

		err = os.MkdirAll(root, 0755) // configurable perms

		if err != nil {
			return err
		}
	}

	err = util.WriteFile(info_path, info)

	if err != nil {
		return err
	}

	sz_params := url.Values{}
	sz_params.Set("photo_id", ph.ID)

	sizes, sz_err := archive.API.ExecuteMethod("flickr.photos.getSizes", sz_params)

	if sz_err != nil {
		return sz_err
	}

	sizes_fname := fmt.Sprintf("%d_%s_s.json", photo_id.Int(), secret.String())
	sizes_path := filepath.Join(root, sizes_fname)

	err = util.WriteFile(sizes_path, sizes)

	if err != nil {
		return err
	}

	sources := gjson.GetBytes(sizes, "sizes.size.#.source")

	if !sources.Exists() {
		return errors.New("Unable to determine sizes")
	}

	err_ch := make(chan error)
	done_ch := make(chan bool)
	count := 0

	for _, url := range sources.Array() {

		remote := url.String()
		fname := filepath.Base(remote)
		local := filepath.Join(root, fname)

		count += 1

		go func(remote string, local string) {

			defer func() {
				done_ch <- true
			}()

			select {
			case <-ctx.Done():
				return
			default:
				err := util.GetStore(remote, local)

				if err != nil {
					err_ch <- err
				}
			}

		}(remote, local)
	}

	var e error

	for count > 0 {

		select {
		case err := <-err_ch:
			ctx.Done()
			e = err
		case <-done_ch:
			count -= 1
		default:
			// pass
		}
	}

	return e
}