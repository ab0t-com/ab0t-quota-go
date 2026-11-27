package handler

func launch(q quotaChecker, req request) error {
	res, err := q.Check(req.ctx, req.input)
	if err != nil {
		return err
	}
	if !res.Allowed() {
		return errOverQuota
	}
	return start(req)
}
