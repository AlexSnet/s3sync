package main

import (
	"bufio"
	"io"
	"log"
	"net/url"
	"os"
	"runtime"
	"sort"
	"sync"

	"github.com/alexsnet/s3sync/util"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"
)

type Handler func(interface{})

type jobQueue struct {
	Handler          Handler
	ConcurrencyLimit int
	push             chan interface{}
	pop              chan struct{}
	suspend          chan bool
	suspended        bool
	stop             chan struct{}
	stopped          bool
	buffer           []interface{}
	count            int
	wg               sync.WaitGroup
}

func NewQueue(handler Handler, concurrencyLimit int) *jobQueue {

	q := &jobQueue{
		Handler:          handler,
		ConcurrencyLimit: concurrencyLimit,
		push:             make(chan interface{}),
		pop:              make(chan struct{}),
		suspend:          make(chan bool),
		stop:             make(chan struct{}),
	}

	go q.run()
	runtime.SetFinalizer(q, (*jobQueue).Stop)
	return q
}

func (q *jobQueue) Push(val interface{}) {
	q.push <- val
}

func (q *jobQueue) Stop() {

	q.stop <- struct{}{}
	runtime.SetFinalizer(q, nil)
}

func (q *jobQueue) Wait() {

	q.wg.Wait()
}

func (q *jobQueue) Len() (_, _ int) {

	return q.count, len(q.buffer)
}

func (q *jobQueue) run() {

	defer func() {
		q.wg.Add(-len(q.buffer))
		q.buffer = nil
	}()
	for {

		select {

		case val := <-q.push:
			q.buffer = append(q.buffer, val)
			q.wg.Add(1)
		case <-q.pop:
			q.count--
		case suspend := <-q.suspend:
			if suspend != q.suspended {

				if suspend {
					q.wg.Add(1)
				} else {
					q.wg.Done()
				}
				q.suspended = suspend
			}
		case <-q.stop:
			q.stopped = true
		}

		if q.stopped && q.count == 0 {

			return
		}

		for (q.count < q.ConcurrencyLimit || q.ConcurrencyLimit == 0) && len(q.buffer) > 0 && !(q.suspended || q.stopped) {

			val := q.buffer[0]
			q.buffer = q.buffer[1:]
			q.count++
			go func() {
				defer func() {
					q.pop <- struct{}{}
					q.wg.Done()
				}()
				q.Handler(val)

			}()
		}

	}

}

func openReader(fp string) (*bufio.Reader, error) {
	fi, _ := os.Stdin.Stat() // get the FileInfo struct describing the standard input.

	if (fi.Mode() & os.ModeCharDevice) == 0 {
		return bufio.NewReader(os.Stdin), nil

	} else {
		f, err := os.Open(fp)
		if err != nil {
			return nil, err
		}
		return bufio.NewReader(f), nil
	}

}

func getAWSSession(sourceUri string) (*session.Session, string, error) {
	uri, err := url.Parse(sourceUri)
	if err != nil {
		return nil, "", err
	}
	pass, _ := uri.User.Password()

	config := aws.NewConfig().WithMaxRetries(3).WithEndpoint(uri.Host)
	config = config.WithCredentials(credentials.NewStaticCredentials(uri.User.Username(), pass, ""))

	if uri.Scheme != "https" {
		config = config.WithDisableSSL(true)
	}

	if region := uri.Query().Get("region"); len(region) > 0 {
		config = config.WithRegion(region)
	}

	sess := session.Must(session.NewSession(config))

	return sess, uri.Path[1:], err
}

func main() {
	app := &cli.App{
		Version: util.Version(),
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "paths",
				Aliases: []string{"p"},
				Usage:   "Load paths `FILE`",
			},

			&cli.IntFlag{
				Name:    "concurrency",
				Aliases: []string{"c"},
				Usage:   "Set number of concurrent streams",
				Value:   runtime.NumCPU(),
			},

			&cli.BoolFlag{
				Name:  "mv",
				Usage: "Delete object in source after transfer to destination",
				Value: false,
			},
			&cli.StringFlag{
				Name:     "src",
				Required: true,
			},
			&cli.StringFlag{
				Name:     "dst",
				Required: true,
			},
			&cli.BoolFlag{
				Name:    "public-read",
				Aliases: []string{"pr"},
				Value:   false,
				Usage:   "Set public-read option on destination",
			},
			&cli.BoolFlag{
				Name:  "tags",
				Value: false,
				Usage: "Transfer objects including user-defined tags",
			},
		},
		Action: func(ctx *cli.Context) error {

			r, err := openReader(ctx.String("paths"))
			if err != nil {
				return cli.Exit("There is no file to read", 86)
			}

			ass, sb, err := getAWSSession(ctx.String("src"))
			if err != nil {
				cli.Exit("Can not configure client for source", 91)
			}

			asd, db, err := getAWSSession(ctx.String("dst"))
			if err != nil {
				cli.Exit("Can not configure client for destination", 92)
			}

			// mcs, sb, err := getMC(ctx.String("src"))
			// if err != nil {
			// 	cli.Exit("Can not configure client for source", 91)
			// }
			// mcd, db, err := getMC(ctx.String("dst"))
			// if err != nil {
			// 	cli.Exit("Can not configure client for destination", 92)
			// }

			damn := ctx.Bool("mv")
			publicread := ctx.Bool("public-read")
			transferTags := ctx.Bool("tags")

			defaultPutACL := "authenticated-read"
			if publicread {
				defaultPutACL = "public-read"
			}

			f := func(s interface{}) {
				line := string(s.(string))
				if line[0] == '/' {
					line = line[1:]
				}

				sourceSession := s3.New(ass)
				result, err := sourceSession.GetObject(&s3.GetObjectInput{
					Bucket: aws.String(sb),
					Key:    aws.String(line),
				})
				if err != nil {
					logrus.WithField("file", line).WithError(err).Error("can not get object from source")
					return
				}
				defer result.Body.Close()

				objectTags := ""
				if transferTags {
					got, err := sourceSession.GetObjectTagging(&s3.GetObjectTaggingInput{
						Bucket: aws.String(sb),
						Key:    aws.String(line),
					})
					if err != nil {
						logrus.WithField("file", line).WithError(err).Error("can not get object tagging information from source")
					}
					var v url.Values
					for _, t := range got.TagSet {
						v.Add(*t.Key, *t.Value)
					}
					objectTags = v.Encode()
				}

				destSession := s3.New(asd)
				pi := &s3.PutObjectInput{
					Body:            aws.ReadSeekCloser(result.Body),
					Bucket:          aws.String(db),
					Key:             aws.String(line),
					ACL:             aws.String(defaultPutACL),
					ContentType:     result.ContentType,
					ContentEncoding: result.ContentEncoding,
					ContentLength:   result.ContentLength,
					Metadata:        result.Metadata,
					Tagging:         aws.String(objectTags),
				}
				dresult, err := destSession.PutObject(pi)
				if err != nil {
					logrus.WithField("file", line).WithError(err).Error("can not write object to destination")
					return
				}
				_ = dresult

				logrus.WithField("file", line).Info("complete")

				if damn {
					_, err := sourceSession.DeleteObject(&s3.DeleteObjectInput{
						Bucket: aws.String(sb),
						Key:    aws.String(line),
					})
					if err != nil {
						logrus.WithField("file", line).WithError(err).Error("can not delete object from source")
					} else {
						logrus.WithField("file", line).Info("removed from source")
					}
				}
			}

			q := NewQueue(f, ctx.Int("concurrency"))

			fileScanner := bufio.NewScanner(r)
			fileScanner.Split(bufio.ScanLines)

			for fileScanner.Scan() {
				if err != nil {
					if err == io.EOF {
						break
					}
					panic(err)
				}

				q.Push(fileScanner.Text())
			}

			q.Wait()

			return nil
		},
	}

	sort.Sort(cli.FlagsByName(app.Flags))
	sort.Sort(cli.CommandsByName(app.Commands))

	err := app.Run(os.Args)
	if err != nil {
		log.Fatal(err)
	}
}
