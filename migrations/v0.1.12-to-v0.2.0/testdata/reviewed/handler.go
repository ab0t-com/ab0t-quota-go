package handler

func launch(q quotaChecker, req request) error {
	res, err := q.Check(req.ctx, req.input)
	if err != nil {
		return err
	}
	// quotago:v0.2.0-reviewed — launches cost money: an unresolved tier must be refused
	if !res.Allowed() {
		return errOverQuota
	}
	return start(req)
}
