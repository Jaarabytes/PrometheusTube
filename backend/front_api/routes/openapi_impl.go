package routes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/eko/gocache/lib/v4/store"
	"github.com/eko/gocache/store/redis/v4"
	schedulerproto "github.com/horahoradev/horahora/scheduler/protocol"

	userproto "github.com/KIRAKIRA-DOUGA/KIRAKIRA-golang-backend/user_service/protocol"
	videoproto "github.com/KIRAKIRA-DOUGA/KIRAKIRA-golang-backend/video_service/protocol"
	"github.com/davegardnerisme/deephash"
	"github.com/labstack/echo/v4"
	"github.com/labstack/gommon/log"
	"github.com/zhenghaoz/gorse/client"
)

type Server struct {
	r RouteHandler
	c *redis.RedisStore
}

func (s Server) Login(ctx echo.Context, params LoginParams) error {
	username := params.Username
	password := params.Password

	// TODO: grpc auth goes here
	loginReq := &userproto.LoginRequest{
		Username: username,
		Password: password,
	}

	loginResp, err := s.r.u.Login(context.Background(), loginReq)
	if err != nil {
		return echo.NewHTTPError(http.StatusForbidden, "Login failed.")
	}

	return setCookie(ctx, loginResp.Jwt)
}

func (s Server) Logout(ctx echo.Context) error {
	cookie := new(http.Cookie)
	cookie.Name = "jwt"
	cookie.Value = ""

	ctx.SetCookie(cookie)

	return ctx.JSON(http.StatusOK, nil)
}

func (s Server) Register(ctx echo.Context, params RegisterParams) error {
	username := params.Username
	password := params.Password
	email := params.Email

	registrationReq := userproto.RegisterRequest{
		Password:         password,
		Username:         username,
		Email:            email,
		VerificationCode: int64(params.VerificationCode),
	}

	regisResp, err := s.r.u.Register(context.Background(), &registrationReq)
	switch {
	case err != nil && strings.Contains(err.Error(), "invalid"): // no, don't do this TODO FIXME
		return echo.NewHTTPError(http.StatusForbidden, err.Error())
	case err != nil:
		return echo.NewHTTPError(http.StatusForbidden, "Registration failed.")
	}

	profile, err := s.r.getUserProfileInfo(ctx)
	if err == nil {
		// TODO hardcoded
		gorse := client.NewGorseClient("http://gorse:8088", "api_key")
		_, err := gorse.InsertUser(context.TODO(), client.User{
			UserId: fmt.Sprintf("%d", profile.UserID),
		}) // send read information to backend
		if err != nil {
			log.Errorf("Failed to insert gorse feedback. Err: %v", err)
		}
	}

	// NO!!! FIXME
	validateResp, err := s.r.u.ValidateJWT(context.TODO(), &userproto.ValidateJWTRequest{
		Jwt: regisResp.Jwt,
	})
	if err != nil {
		return echo.NewHTTPError(http.StatusForbidden, "Authentication failed after registering; forward this to OtoMAN immediately (?!)")
	}

	_, err = s.r.u.AddAuditEvent(context.TODO(), &userproto.NewAuditEventRequest{
		Message: "New user has registered",
		User_ID: validateResp.Uid,
	})
	if err != nil {
		return err // If the audit event can't be created, fail the operation
	}

	// TODO: use registration JWT to auth

	return setCookie(ctx, regisResp.Jwt)
}

func (s Server) Upload(ctx echo.Context, params UploadParams) error {
	profile, err := s.r.getUserProfileInfo(ctx)
	if err != nil {
		return err
	}

	if profile.Rank != 2 {
		return ctx.String(http.StatusForbidden, "Insufficient user status")
	}

	title := params.Title
	description := params.Description
	tags := params.Tags

	thumbFileHeader, err := ctx.FormFile(thumbnailKey)
	if err != nil {
		return err
	}

	thumbFile, err := thumbFileHeader.Open()
	if err != nil {
		return err
	}

	videoFileHeader, err := ctx.FormFile(videoKey)
	if err != nil {
		return err
	}

	videoFile, err := videoFileHeader.Open()
	if err != nil {
		return err
	}

	// TODO: rewrite me, this isn't memory efficient
	videoBytes, err := ioutil.ReadAll(videoFile)
	if err != nil {
		return err
	}

	thumbBytes, err := ioutil.ReadAll(thumbFile)
	if err != nil {
		return err
	}

	uploadClient, err := s.r.v.UploadVideo(context.Background())
	if err != nil {
		return err
	}

	metaChunk := &videoproto.InputVideoChunk{
		Payload: &videoproto.InputVideoChunk_Meta{
			Meta: &videoproto.InputFileMetadata{
				Title:             title,
				Description:       description,
				AuthorUID:         "0", // TODO can't accept a blank foreign author id??
				OriginalVideoLink: "0",
				AuthorUsername:    profile.Username,
				OriginalSite:      "blank", // todo AAAAAAAAAAAAAAAAAa
				OriginalID:        "0",
				DomesticAuthorID:  profile.UserID,
				Tags:              tags,
				Thumbnail:         thumbBytes,
				Category:          params.Category,
			},
		},
	}

	err = uploadClient.Send(metaChunk)
	if err != nil {
		return err
	}

	for byteInd := 0; byteInd < len(videoBytes); byteInd += fileUploadChunkSize {
		videoByteSlice := videoBytes[byteInd:min(len(videoBytes), byteInd+fileUploadChunkSize)]
		log.Infof("uploading byte %d", byteInd)
		videoChunk := &videoproto.InputVideoChunk{
			Payload: &videoproto.InputVideoChunk_Content{
				Content: &videoproto.FileContent{
					Data: videoByteSlice,
				},
			},
		}

		err = uploadClient.Send(videoChunk)
		if err != nil {
			return err
		}
	}

	resp, err := uploadClient.CloseAndRecv()
	if err != nil {
		return err
	}

	// Redirect to the new video
	return ctx.JSON(http.StatusOK, resp.VideoID)
}

func (s Server) readThroughCache(i interface{}) (interface{}, error) {
	structHash := deephash.Hash(i)
	return s.c.Get(context.TODO(), string(structHash))
}

func (s Server) writeThroughCache(i, r interface{}) error {
	structHash := deephash.Hash(i)
	payload, err := json.Marshal(r)
	if err != nil {
		return err
	}
	s.c.Set(context.TODO(), string(structHash), payload, store.WithExpiration(1*time.Minute))
	return nil
}

func (s Server) Videos(ctx echo.Context, params VideosParams) error {
	r, err := s.readThroughCache(params)
	if err == nil {
		log.Error("Cache hit!")
		r, ok := r.(string)
		if !ok {
			return fmt.Errorf("Failed to typecast to str. Content: %v", r)
		}
		return ctx.JSONBlob(http.StatusOK, []byte(r))
	} else {
		log.Errorf("Cache miss: %v", err)
	}

	search := params.Search
	if search == nil || string(*search) == "none" { // TODO
		v := []byte{}
		search = &v
	}

	showUnapproved := true

	orderByVal := params.SortCategory

	unapprovedVal := params.Unapproved
	unapprovedBool := false // lmao fixme
	if unapprovedVal != nil && *unapprovedVal == "true" {
		unapprovedBool = true
	}

	category := params.Category
	if params.Category == nil || string(*params.Category) == "undefined" {
		w := []byte{}
		category = &w
	}

	// Default
	if orderByVal == nil || *orderByVal == "undefined" {
		v := "upload_date"
		orderByVal = &v
	}

	orderCat, ok := videoproto.OrderCategory_value[*orderByVal]
	if !ok {
		return fmt.Errorf("invalid category supplied: %s", *orderByVal)
	}

	orderBy := videoproto.OrderCategory(orderCat)

	var order videoproto.SortDirection
	if params.Order == nil {
		order = videoproto.SortDirection_desc // Default to desc
	} else {
		order = videoproto.SortDirection(videoproto.SortDirection_value[*params.Order])
	}

	// TODO need default, maybe in openapi defs?
	var pageNumber = 1
	if params.PageNumber != nil {
		pageNumber = *params.PageNumber
	}

	// TODO: if request times out, maybe provide a default list of good videos
	// TODO audit all of these nil ptr derefs (high priority)
	req := videoproto.VideoQueryConfig{
		OrderBy:        orderBy,
		Direction:      order,
		SearchVal:      string(*search),
		PageNumber:     int64(pageNumber),
		ShowUnapproved: showUnapproved,
		UnapprovedOnly: unapprovedBool,
		Category:       string(*category),
	}

	videoList, err := s.r.v.GetVideoList(context.TODO(), &req)
	if err != nil {
		log.Errorf("Could not retrieve video list. Err: %s", err)
		return errors.New("Could not retrieve video list")
	}

	var cats []Category
	for _, cat := range videoList.Categories.Categories {
		cats = append(cats, Category{
			Name:        cat.Name,
			Cardinality: int(cat.Cardinality),
		})
	}

	data := HomePageData{
		PaginationData: PaginationData{
			NumberOfItems: int(videoList.NumberOfVideos),
			CurrentPage:   pageNumber,
		},
		Categories: cats,
	}

	data.Videos = []Video{}
	for _, video := range videoList.Videos {
		data.Videos = append(data.Videos, Video{
			Title:         video.VideoTitle,
			VideoID:       video.VideoID,
			Views:         video.Views,
			AuthorID:      video.AuthorID, // TODO
			AuthorName:    video.AuthorName,
			ThumbnailLoc:  video.ThumbnailLoc,
			Rating:        video.Rating,
			VideoDuration: video.VideoDuration,
		})
	}

	err = s.writeThroughCache(params, data)
	if err != nil {
		log.Errorf("Failed to set cache: %v", err)
	}

	return ctx.JSON(http.StatusOK, data)
}

func (s Server) VideoDetail(ctx echo.Context, id float32) error {
	videoID := int64(id)

	go func() {
		profile, err := s.r.getUserProfileInfo(ctx)
		if err == nil {
			// TODO hardcoded
			gorse := client.NewGorseClient("http://gorse:8088", "api_key")
			_, err := gorse.InsertFeedback(context.TODO(), []client.Feedback{
				{FeedbackType: "read", UserId: fmt.Sprintf("%d", profile.UserID), ItemId: fmt.Sprintf("%d", videoID), Timestamp: time.Now().String()},
			}) // send read information to backend
			if err != nil {
				log.Errorf("Failed to insert gorse feedback. Err: %v", err)
			}
		}
		// Increment views first
		viewReq := videoproto.VideoViewing{VideoID: videoID}
		_, err = s.r.v.ViewVideo(context.Background(), &viewReq)
		if err != nil {
			log.Errorf("Failed to view video. Err: %v", err)
		}
	}()

	r, err := s.readThroughCache(id)
	if err == nil {
		r, ok := r.(string)
		if !ok {
			return fmt.Errorf("Failed to typecast to str. Content: %v", r)
		}
		return ctx.JSONBlob(http.StatusOK, []byte(r))
	} else {
		log.Errorf("Cache miss: %v", err)
	}

	videoReq := videoproto.VideoRequest{
		VideoID: fmt.Sprintf("%d", videoID),
	}

	videoInfo, err := s.r.v.GetVideo(context.Background(), &videoReq)
	if err != nil {
		return err // Get comments for video ID
		// (GET /comments/{id})
	}

	rating := videoInfo.Rating

	data := VideoDetail{
		Title:            videoInfo.VideoTitle,
		MPDLoc:           videoInfo.VideoLoc, // FIXME: fix this in videoservice LOL this is embarrassing
		Views:            videoInfo.Views,
		Rating:           rating,
		AuthorID:         videoInfo.AuthorID, // TODO
		Username:         videoInfo.AuthorName,
		UserDescription:  "", // TODO: not implemented yet
		VideoDescription: videoInfo.Description,
		UserSubscribers:  0, // TODO: not implemented yet
		ProfilePicture:   "/static/images/placeholder1.jpg",
		UploadDate:       videoInfo.UploadDate,
		VideoID:          videoInfo.VideoID,
		Tags:             videoInfo.Tags,
		VideoDuration:    videoInfo.VideoDuration,
		Thumbnail:        videoInfo.Thumbnail,

		// L: profile,
	}

	err = s.writeThroughCache(id, data)
	if err != nil {
		log.Errorf("Failed to set cache: %v", err)
	}

	return ctx.JSON(http.StatusOK, data)
}

func (s Server) Comment(c echo.Context, params CommentParams) error {
	videoIDInt := params.VideoID
	content := params.Content
	parentIDInt := params.Parent

	profile, err := s.r.getUserProfileInfo(c)
	if err != nil {
		return err
	}

	_, err = s.r.v.MakeComment(context.Background(), &videoproto.VideoComment{
		UserId:        profile.UserID,
		VideoId:       int64(videoIDInt),
		Comment:       string(content),
		ParentComment: int64(parentIDInt),
	})
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, nil)
}

func (s Server) Comments(c echo.Context, videoIDInt int) error {
	var uid int64 = -1

	profileInfo, err := s.r.getUserProfileInfo(c)
	if err == nil {
		uid = profileInfo.UserID
		log.Infof("fetching comments for user %d", uid)
	} else {
		log.Errorf("Failed to get user profile. Err: %v", err)
	}

	resp, err := s.r.v.GetCommentsForVideo(context.Background(), &videoproto.CommentRequest{VideoID: int64(videoIDInt), CurrUserID: uid})
	if err != nil {
		return c.String(http.StatusInternalServerError, err.Error())
	}

	commentList := make([]CommentData, 0)

	for _, comment := range resp.Comments {
		commentData := CommentData{
			ID:                    comment.CommentId,
			CreationDate:          comment.CreationDate,
			Content:               comment.Content,
			Username:              comment.AuthorUsername,
			ProfileImage:          comment.AuthorProfileImageUrl,
			VoteScore:             comment.VoteScore,
			CurrUserHasUpvoted:    comment.CurrentUserHasUpvoted,
			CurrUserHasDownvoted:  comment.CurrentUserHasDownvoted,
			AuthoredByCurrentUser: comment.AuthorId == uid,
		}

		if comment.ParentId != 0 {
			commentData.ParentID = comment.ParentId
		}

		commentList = append(commentList, commentData)
	}

	return c.JSON(http.StatusOK, &commentList)
}

func (s Server) Users(c echo.Context, idInt int) error {
	getUserReq := userproto.GetUserFromIDRequest{UserID: int64(idInt)}

	profile, err := s.r.u.GetUserFromID(context.TODO(), &getUserReq)
	if err != nil {
		return fmt.Errorf("Get user from ID: %s", err)
	}

	pageNumber := getPageNumber(c)

	videoQueryConfig := videoproto.VideoQueryConfig{
		OrderBy:        videoproto.OrderCategory_upload_date,
		Direction:      videoproto.SortDirection_desc,
		PageNumber:     pageNumber,
		SearchVal:      "",
		FromUserID:     int64(idInt),
		ShowUnapproved: true,
	}

	videoList, err := s.r.v.GetVideoList(context.TODO(), &videoQueryConfig)
	if err != nil {
		return fmt.Errorf("Get video list: %s", err)
	}

	// TODO: 0 results in all videos, fix for admin user?
	data := ProfileData{
		UserID:            int64(idInt),
		Username:          profile.Username,
		Gender:            profile.Gender,
		Bio:               profile.Bio,
		Birthdate:         profile.Birthdate,
		JoinDate:          profile.JoinDate,
		L:                 nil,
		ProfilePictureURL: "/static/images/placeholder1.jpg",
		PaginationData: PaginationData{
			NumberOfItems: int(videoList.NumberOfVideos),
			CurrentPage:   int(pageNumber),
		},
		Banned: profile.Banned,
	}

	data.Videos = []Video{}
	for _, video := range videoList.Videos {
		v := Video{
			Title:         video.VideoTitle,
			VideoID:       video.VideoID,
			Views:         video.Views,
			AuthorID:      0, // TODO
			AuthorName:    video.AuthorName,
			ThumbnailLoc:  video.ThumbnailLoc,
			Rating:        video.Rating,
			VideoDuration: video.VideoDuration,
		}

		data.Videos = append(data.Videos, v)
	}

	return c.JSON(http.StatusOK, data)
}

// TODO: better input validation
func (s Server) Upvote(c echo.Context, commentID int, params UpvoteParams) error {
	profile, err := s.r.getUserProfileInfo(c)
	if err != nil {
		return err
	}

	_, err = s.r.v.MakeCommentUpvote(context.Background(), &videoproto.CommentUpvote{
		CommentId: int64(commentID),
		UserId:    profile.UserID,
		Score:     int64(params.Score),
	})
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, nil)
}

func (s Server) DeleteComment(c echo.Context, params DeleteCommentParams) error {
	profile, err := s.r.getUserProfileInfo(c)
	if err != nil {
		return err
	}

	_, err = s.r.v.DeleteComment(context.Background(), &videoproto.CommentDeletionReq{
		CommentID: int64(params.Id),
		UserID:    profile.UserID,
	})
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, nil)
}

func (s Server) ResetPassword(c echo.Context, params ResetPasswordParams) error {
	profile, err := s.r.getUserProfileInfo(c)
	if err != nil {
		return err
	}

	oldPass := params.Oldpassword
	newPass := params.Newpassword

	// TODO: grpc auth goes here
	resetReq := userproto.ResetPasswordRequest{
		UserID:      profile.UserID,
		OldPassword: oldPass,
		NewPassword: newPass,
	}

	_, err = s.r.u.ResetPassword(context.TODO(), &resetReq)
	return err
}

func (s Server) UpvoteVideo(c echo.Context, id int, params UpvoteVideoParams) error {
	videoIDInt := id
	rating := params.Score

	profile, err := s.r.getUserProfileInfo(c)
	if err != nil {
		return err
	}

	if params.Score != -1 && params.Score != 1 {
		return errors.New("bad score value")
	}

	if params.Score == 1 {
		gorse := client.NewGorseClient("http://gorse:8088", "api_key")
		_, err := gorse.InsertFeedback(context.TODO(), []client.Feedback{
			{
				FeedbackType: "upvote",
				UserId:       fmt.Sprintf("%d", profile.UserID),
				ItemId:       fmt.Sprintf("%d", id),
				Timestamp:    time.Now().String(),
			},
		}) // send read information to backend
		if err != nil {
			log.Errorf("Failed to insert gorse feedback. Err: %v", err)
		}
	}

	rateReq := videoproto.VideoRating{
		UserID:  profile.UserID,
		VideoID: int64(videoIDInt),
		Rating:  int64(rating),
	}

	_, err = s.r.v.RateVideo(context.TODO(), &rateReq)
	if err != nil {
		return err
	}

	return c.JSON(http.StatusOK, nil)
}

func (s Server) Recommendations(c echo.Context, id int) error {
	profile, _ := s.r.getUserProfileInfo(c)

	var userID int64 = 0
	if profile != nil {
		userID = profile.UserID
	}
	recResp, err := s.r.v.GetVideoRecommendations(context.Background(), &videoproto.RecReq{
		UserId:  userID,
		VideoId: int64(id),
	})
	if err != nil {
		return err
	}

	recVideos := []Video{}
	if recResp != nil {
		for _, rec := range recResp.Videos {
			// FIXME: fill other fields after modifying protocol
			vid := Video{
				Title:         rec.VideoTitle,
				VideoID:       rec.VideoID,
				ThumbnailLoc:  rec.ThumbnailLoc,
				Views:         rec.Views,
				Rating:        rec.Rating,
				AuthorName:    rec.AuthorName,
				VideoDuration: rec.VideoDuration,
				AuthorID:      rec.AuthorID,
			}

			recVideos = append(recVideos, vid)
		}
	}

	return c.JSON(http.StatusOK, recVideos)
}

func (s Server) FollowFeed(ctx echo.Context) error {
	profile, err := s.r.getUserProfileInfo(ctx)
	if err != nil {
		return err
	}

	users, err := s.r.u.GetFollowers(context.Background(), &userproto.FollowerReq{
		User_ID: profile.UserID,
	})

	videos, err := s.r.v.GetFollowFeed(context.Background(), &videoproto.FeedReq{
		FollowedUsers: users.Users,
	})
	if err != nil {
		return err
	}

	recVideos := []Video{}
	if videos != nil {
		for _, rec := range videos.Videos {
			// FIXME: fill other fields after modifying protocol
			vid := Video{
				Title:         rec.VideoTitle,
				VideoID:       rec.VideoID,
				ThumbnailLoc:  rec.ThumbnailLoc,
				Views:         rec.Views,
				Rating:        rec.Rating,
				AuthorName:    rec.AuthorName,
				VideoDuration: rec.VideoDuration,
				AuthorID:      rec.AuthorID,
			}

			recVideos = append(recVideos, vid)
		}
	}

	return ctx.JSON(http.StatusOK, recVideos)
}

func (s Server) Follow(ctx echo.Context, id int) error {
	profile, err := s.r.getUserProfileInfo(ctx)
	if err != nil {
		return err
	}

	_, err = s.r.u.AddFolllower(context.TODO(), &userproto.AddFollowReq{
		FollowingId: profile.UserID,
		FollowedId:  int64(id),
	})

	return ctx.JSON(http.StatusOK, nil)
}

func (s Server) UpdateProfile(ctx echo.Context, params UpdateProfileParams) error {
	profile, err := s.r.getUserProfileInfo(ctx)
	if err != nil {
		return err
	}

	_, err = s.r.u.SetProfile(context.TODO(), &userproto.ProfileReq{
		Username:  string(params.Username),
		Birthdate: params.Birthdate,
		Bio:       string(params.Bio),
		Gender:    string(params.Gender),
		User_ID:   profile.UserID,
	})
	if err != nil {
		return err
	}

	return ctx.JSON(http.StatusOK, nil)
}

type Danmaku struct {
	AuthorID     *int64  `json:"AuthorID,omitempty"`
	Color        *string `json:"Color,omitempty"`
	CreationDate *string `json:"CreationDate,omitempty"`
	FontSize     *string `json:"FontSize,omitempty"`
	ID           *int64  `json:"ID,omitempty"`
	Message      *string `json:"Message,omitempty"`
	Timestamp    *string `json:"Timestamp,omitempty"`
	Type         *string `json:"Type,omitempty"`
}

func (s Server) GetDanmaku(ctx echo.Context, id int) error {
	resp, err := s.r.v.GetDanmaku(context.TODO(), &videoproto.DanmakuQueryReq{
		VideoId: int64(id),
	})
	if err != nil {
		return err
	}

	var finResp []*Danmaku

	for _, dn := range resp.Comments {
		dan := Danmaku{
			AuthorID:  &dn.AuthorId,
			Color:     &dn.Color,
			FontSize:  &dn.FontSize,
			ID:        &dn.Id,
			Message:   &dn.Message,
			Timestamp: &dn.Timestamp,
			Type:      &dn.Type,
		}
		finResp = append(finResp, &dan)
	}

	return ctx.JSON(http.StatusOK, finResp)
}

func (s Server) CreateDanmaku(ctx echo.Context, params CreateDanmakuParams) error {
	profile, err := s.r.getUserProfileInfo(ctx)
	if err != nil {
		return err
	}

	_, err = s.r.v.AddDanmaku(context.TODO(), &videoproto.Danmaku{
		VideoId:   int64(params.VideoID),
		Timestamp: params.Timestamp,
		Message:   string(params.Message),
		AuthorId:  profile.UserID,
		Type:      params.Type,
		Color:     params.Color,
		FontSize:  params.FontSize,
	})
	if err != nil {
		return err
	}

	return ctx.JSON(http.StatusOK, nil)
}

func (s Server) EmailValidation(ctx echo.Context, params EmailValidationParams) error {
	_, err := s.r.u.EmailValidation(context.TODO(), &userproto.ValidationRequest{
		Email: params.Email,
	})
	if err != nil {
		return err
	}
	return nil
}

func (s Server) ArchiveEvents(ctx echo.Context, params *ArchiveEventsParams) error {
	downloadID, err := url.QueryUnescape(c.Param("id"))
	if err != nil {
		return ctx.String(http.StatusInternalServerError, err.Error())
	}

	if downloadID == "all" {
		data := ArchiveEventsData{}

		resp, err := s.r.s.ListArchivalEvents(context.TODO(), &schedulerproto.ListArchivalEventsRequest{DownloadID: 0, ShowAll: true})
		if err != nil {
			return err
		}

		data.ArchivalEvents = resp.Events
		return ctx.JSON(http.StatusOK, data)
	} else {
		downloadIDInt, err := strconv.ParseInt(downloadID, 10, 64)
		if err != nil {
			return ctx.String(http.StatusInternalServerError, err.Error())
		}

		data := ArchiveEventsData{}

		resp, err := s.r.s.ListArchivalEvents(context.TODO(), &schedulerproto.ListArchivalEventsRequest{DownloadID: downloadIDInt})
		if err != nil {
			return err
		}

		data.ArchivalEvents = resp.Events

		return ctx.JSON(http.StatusOK, data)
	}
}
