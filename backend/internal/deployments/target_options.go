package deployments

import "context"

// TargetOption is the read-only subset a deploy form needs. Every
// authenticated role may read it: the same name, environment and endpoint are
// already stored on each deployment snapshot, and only active targets can
// receive a deployment. Secrets, healthPath, deployPath and the disabled
// targets stay admin-only.
type TargetOption struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Environment string `json:"environment"`
	Endpoint    string `json:"endpoint"`
}

// TargetOptions lists the active targets a deployment can be created against,
// ordered by environment then name.
func (s *Service) TargetOptions(ctx context.Context) ([]TargetOption, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,environment,endpoint FROM deployment_targets
		WHERE status=$1 ORDER BY environment,name`, StatusActive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	options := make([]TargetOption, 0)
	for rows.Next() {
		var option TargetOption
		if err := rows.Scan(&option.ID, &option.Name, &option.Environment, &option.Endpoint); err != nil {
			return nil, err
		}
		options = append(options, option)
	}
	return options, rows.Err()
}
